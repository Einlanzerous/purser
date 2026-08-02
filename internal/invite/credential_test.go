package invite

import (
	"context"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/connectors/cloudflare"
	"github.com/Einlanzerous/purser/internal/model"
)

const testLauncher = "https://zero-gravity-industries.cloudflareaccess.com"

func testPerson() model.Person {
	return model.Person{Name: "Ada Lovelace", Email: "ada@example.com", Type: model.PersonHuman}
}

func cfOutcome(status model.TaskStatus) ServiceOutcome {
	return ServiceOutcome{
		ServiceKey:   "cloudflare",
		DisplayName:  "Cloudflare Access (SSO)",
		Icon:         "🔐",
		Status:       status,
		Instructions: "Sign in with the email one-time-PIN.",
	}
}

func argosyOutcome() ServiceOutcome {
	return ServiceOutcome{
		ServiceKey:  "argosy",
		DisplayName: "Argosy",
		Icon:        "🎬",
		Status:      model.TaskSucceeded,
		Username:    "ada@example.com",
		Secret:      "hunter2",
		SecretLabel: "password (shown once — change it after signing in)",
		LoginURL:    "https://argosy.zerogravity.industries",
	}
}

// The launcher is the call to action when the invite actually granted Access.
func TestRenderCredentialBlockLeadsWithLauncher(t *testing.T) {
	block := RenderCredentialBlock(testPerson(), []ServiceOutcome{
		cfOutcome(model.TaskSucceeded),
		argosyOutcome(),
	}, testLauncher)

	if !strings.Contains(block, testLauncher) {
		t.Fatalf("expected launcher URL in block:\n%s", block)
	}
	// "Leads with" is the point — it has to land before the per-app details, or
	// it's just another line in a list.
	launcherAt := strings.Index(block, testLauncher)
	argosyAt := strings.Index(block, "Argosy")
	if launcherAt < 0 || argosyAt < 0 || launcherAt > argosyAt {
		t.Fatalf("launcher should precede the per-service details:\n%s", block)
	}
	if !strings.Contains(block, "ada@example.com — no password") {
		t.Errorf("expected the OTP sign-in note naming the person's email:\n%s", block)
	}
}

// An Argosy-only invitee has no Access grant, so the launcher would render them
// an empty page. Linking it would read as a broken invite.
func TestRenderCredentialBlockOmitsLauncherWithoutAccessGrant(t *testing.T) {
	block := RenderCredentialBlock(testPerson(), []ServiceOutcome{argosyOutcome()}, testLauncher)

	if strings.Contains(block, testLauncher) {
		t.Fatalf("launcher must not appear without a cloudflare grant:\n%s", block)
	}
	if !strings.Contains(block, "granted access to the following") {
		t.Errorf("expected the plain per-service greeting:\n%s", block)
	}
	// The one-time secret still has to be delivered inline — that's the whole
	// reason a direct-path app can't be launcher-only.
	if !strings.Contains(block, "hunter2") {
		t.Errorf("expected the Argosy password inline:\n%s", block)
	}
}

// Already having the grant is still having it.
func TestRenderCredentialBlockShowsLauncherForSkippedAccess(t *testing.T) {
	block := RenderCredentialBlock(testPerson(), []ServiceOutcome{
		cfOutcome(model.TaskSkipped),
		argosyOutcome(),
	}, testLauncher)

	if !strings.Contains(block, testLauncher) {
		t.Fatalf("a skipped (already-provisioned) cloudflare grant still reaches the launcher:\n%s", block)
	}
}

// A failed Access task means they cannot sign in yet.
func TestRenderCredentialBlockOmitsLauncherWhenAccessFailed(t *testing.T) {
	failed := cfOutcome(model.TaskFailed)
	failed.Error = "cloudflare: 403"
	block := RenderCredentialBlock(testPerson(), []ServiceOutcome{failed, argosyOutcome()}, testLauncher)

	if strings.Contains(block, testLauncher) {
		t.Fatalf("launcher must not appear when the cloudflare grant failed:\n%s", block)
	}
}

// No configured launcher => the block is exactly what it was before.
func TestRenderCredentialBlockWithoutLauncherURL(t *testing.T) {
	block := RenderCredentialBlock(testPerson(), []ServiceOutcome{cfOutcome(model.TaskSucceeded)}, "")

	if strings.Contains(block, "Start here") {
		t.Fatalf("no launcher configured, so no call to action:\n%s", block)
	}
	if !strings.Contains(block, "granted access to the following") {
		t.Errorf("expected the plain per-service greeting:\n%s", block)
	}
}

// The half-open case: Access succeeded but an app-side grant didn't. The person
// would clear the edge gate and then be refused by the app, so the block must
// not present that as a finished welcome.
func TestRenderCredentialBlockOmitsLauncherWhenAnotherServiceFailed(t *testing.T) {
	sw := ServiceOutcome{
		ServiceKey: "switchyard", DisplayName: "Switchyard", Icon: "🚉",
		Status: model.TaskFailed, Error: "switchyard: 500",
	}
	block := RenderCredentialBlock(testPerson(), []ServiceOutcome{
		cfOutcome(model.TaskSucceeded), sw,
	}, testLauncher)

	if strings.Contains(block, testLauncher) {
		t.Fatalf("a half-provisioned invite must not lead with the launcher:\n%s", block)
	}
	if !strings.Contains(block, "granted access to the following") {
		t.Errorf("expected the plain per-service greeting:\n%s", block)
	}
}

// The launcher header already explains the email-OTP sign-in; a standalone
// Cloudflare entry would say it a second time under its own heading.
func TestRenderCredentialBlockDoesNotRepeatTheAccessEntry(t *testing.T) {
	block := RenderCredentialBlock(testPerson(), []ServiceOutcome{
		cfOutcome(model.TaskSucceeded), argosyOutcome(),
	}, testLauncher)

	if strings.Contains(block, "Cloudflare Access (SSO)") {
		t.Errorf("launcher header replaces the Cloudflare entry:\n%s", block)
	}
	if n := strings.Count(block, "one-time-PIN"); n != 1 {
		t.Errorf("sign-in instruction appears %d times, want 1:\n%s", n, block)
	}
	// Suppression must not touch the caller's slice or the other services.
	if !strings.Contains(block, "Argosy") {
		t.Errorf("other services must still render:\n%s", block)
	}
}

// The credential block is the recipient's message; the failure list is the
// operator's. They are rendered separately so that handing the block to an
// invitee — by paste or by email — cannot carry connector error text with it.
func TestRenderCredentialBlockExcludesTheOperatorNote(t *testing.T) {
	failed := ServiceOutcome{
		ServiceKey: "switchyard", DisplayName: "Switchyard", Icon: "🚉",
		Status: model.TaskFailed, Error: "switchyard: 500",
	}
	outcomes := []ServiceOutcome{argosyOutcome(), failed}

	block := RenderCredentialBlock(testPerson(), outcomes, testLauncher)
	if strings.Contains(block, "Operator note") || strings.Contains(block, "switchyard: 500") {
		t.Errorf("the recipient's block must not carry the failure list:\n%s", block)
	}

	note := RenderOperatorNote(outcomes)
	if !strings.Contains(note, "switchyard: 500") {
		t.Errorf("operator note missing the failure:\n%s", note)
	}
	// Only failures — the operator reads this to see what needs a retry.
	if strings.Contains(note, "Argosy") {
		t.Errorf("operator note should list failures only:\n%s", note)
	}
}

// An empty note is how the CLI and API tell "nothing broke" from "something did"
// without re-walking the outcomes.
func TestRenderOperatorNoteEmptyWhenNothingFailed(t *testing.T) {
	note := RenderOperatorNote([]ServiceOutcome{argosyOutcome(), cfOutcome(model.TaskSkipped)})
	if note != "" {
		t.Errorf("nothing failed, so there is nothing to tell the operator:\n%s", note)
	}
}

// A connector that is wired but not yet ready upstream is not a breakage, and
// the note says so — retrying it won't help.
func TestRenderOperatorNoteMarksPendingSeparately(t *testing.T) {
	pending := ServiceOutcome{
		ServiceKey: "lyceum", DisplayName: "Lyceum", Status: model.TaskFailed,
		Error: "lyceum: connector not configured", Pending: true,
	}
	note := RenderOperatorNote([]ServiceOutcome{pending})
	if !strings.Contains(note, "(pending)") {
		t.Errorf("a pending connector should read as pending, not failed:\n%s", note)
	}
}

// Without this the launcher silently switches off if the connector's key is ever
// renamed, and every test above still passes because they hardcode the literal.
func TestAccessServiceKeyMatchesTheConnector(t *testing.T) {
	got := cloudflare.New(cloudflare.Config{}).Key()
	if got != AccessServiceKey {
		t.Fatalf("cloudflare connector Key() = %q, but the launcher gates on %q", got, AccessServiceKey)
	}
}

// The renderer tests all call RenderCredentialBlock directly, which would stay
// green if WithLauncher were deleted from the composition root. This exercises
// the option → Service.launcher → Run path end to end.
func TestRunThreadsTheLauncherIntoTheBlock(t *testing.T) {
	st := newFakeStore()
	cf := &fakeConn{
		key: AccessServiceKey, display: "Cloudflare Access (SSO)", icon: "🔐",
		result: connector.Result{ExternalID: "ada@example.com", Instructions: "use the email OTP"},
	}
	reg := connector.NewRegistry(cf)
	svc := New(seededStore(t, st, reg), reg, nil, WithLauncher(testLauncher))

	res, err := svc.Run(context.Background(), Request{
		Name: "Ada Lovelace", Email: "ada@example.com",
		Services: []string{AccessServiceKey}, Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.CredentialBlock, testLauncher) {
		t.Fatalf("Run did not thread the launcher into the block:\n%s", res.CredentialBlock)
	}
}

// A Service built without the option renders the pre-launcher block.
func TestRunWithoutLauncherOption(t *testing.T) {
	st := newFakeStore()
	cf := &fakeConn{
		key: AccessServiceKey, display: "Cloudflare Access (SSO)", icon: "🔐",
		result: connector.Result{ExternalID: "ada@example.com", Instructions: "use the email OTP"},
	}
	reg := connector.NewRegistry(cf)
	svc := New(seededStore(t, st, reg), reg, nil)

	res, err := svc.Run(context.Background(), Request{
		Name: "Ada Lovelace", Email: "ada@example.com",
		Services: []string{AccessServiceKey}, Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(res.CredentialBlock, "Start here") {
		t.Fatalf("no launcher configured, so no call to action:\n%s", res.CredentialBlock)
	}
}
