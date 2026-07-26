package invite

import (
	"strings"
	"testing"

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
