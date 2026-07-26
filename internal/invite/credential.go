package invite

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Einlanzerous/purser/internal/model"
)

// accessServiceKey is the connector whose grant puts a person into Cloudflare's
// App Launcher. Named here as a string rather than imported from the connector
// package: the orchestrator deals in service keys throughout (bundles are plain
// key lists), and depending on one concrete connector would undo the point of
// the interface.
const accessServiceKey = "cloudflare"

// RenderCredentialBlock builds the copy-pasteable message an operator hands to
// the invited person (or that Purser emails them). It leads with the launcher —
// the one page listing every app they can reach — and then gives the per-service
// login URL, any one-time secret, and how to sign in.
//
// launcher is Cloudflare's App Launcher URL, and is shown only when this invite
// actually granted Access (see hasAccessGrant). Pointing someone at it without
// that grant renders them an empty page, which is worse than not linking it.
//
// The block is plain text on purpose: it pastes cleanly into Discord, Slack,
// SMS, or an email body with no rendering surprises.
func RenderCredentialBlock(person model.Person, outcomes []ServiceOutcome, launcher string) string {
	var b strings.Builder

	greeting := "there"
	if n := strings.TrimSpace(person.Name); n != "" {
		greeting = strings.Fields(n)[0]
	}

	var (
		succeeded []ServiceOutcome
		skipped   []ServiceOutcome
		failed    []ServiceOutcome
	)
	for _, o := range outcomes {
		switch o.Status {
		case model.TaskSucceeded:
			succeeded = append(succeeded, o)
		case model.TaskSkipped:
			skipped = append(skipped, o)
		case model.TaskFailed:
			failed = append(failed, o)
		}
	}

	launcher = strings.TrimSpace(launcher)
	showLauncher := launcher != "" && hasAccessGrant(succeeded, skipped)

	if showLauncher {
		fmt.Fprintf(&b, "Hi %s — you've been granted access to the Construct.\n\n", greeting)
		fmt.Fprintf(&b, "🚀 Start here: %s\n", launcher)
		if email := strings.TrimSpace(person.Email); email != "" {
			fmt.Fprintf(&b, "    Sign in with the email one-time-PIN sent to %s — no password.\n", email)
		} else {
			b.WriteString("    Sign in with the email one-time-PIN — no password.\n")
		}
		b.WriteString("    Every app you can reach is listed on that page.\n")
		b.WriteString("\nPer-app details, including anything the launcher can't sign you into:\n")
	} else {
		fmt.Fprintf(&b, "Hi %s — you've been granted access to the following:\n", greeting)
	}

	for _, o := range succeeded {
		b.WriteString("\n")
		fmt.Fprintf(&b, "%s %s\n", marker(o.Icon), o.DisplayName)
		if o.LoginURL != "" {
			fmt.Fprintf(&b, "    URL:      %s\n", o.LoginURL)
		}
		if o.Username != "" {
			fmt.Fprintf(&b, "    Username: %s\n", o.Username)
		}
		if o.Secret != "" {
			label := o.SecretLabel
			if label == "" {
				label = "Secret"
			}
			fmt.Fprintf(&b, "    %s: %s\n", label, o.Secret)
		}
		for _, k := range sortedKeys(o.Extra) {
			fmt.Fprintf(&b, "    %s: %s\n", k, o.Extra[k])
		}
		if o.Instructions != "" {
			fmt.Fprintf(&b, "    → %s\n", o.Instructions)
		}
	}

	for _, o := range skipped {
		b.WriteString("\n")
		fmt.Fprintf(&b, "%s %s (already set up)\n", marker(o.Icon), o.DisplayName)
		if o.Username != "" {
			fmt.Fprintf(&b, "    Username: %s\n", o.Username)
		}
		if o.Instructions != "" {
			fmt.Fprintf(&b, "    → %s\n", o.Instructions)
		}
	}

	b.WriteString("\nKeep any secrets above private — they are shown once and cannot be retrieved later.\n")

	if len(failed) > 0 {
		b.WriteString("\n(Operator note — not for the recipient)\n")
		for _, o := range failed {
			status := "failed"
			if o.Pending {
				status = "pending"
			}
			fmt.Fprintf(&b, "  ✗ %s: %s (%s)\n", o.DisplayName, o.Error, status)
		}
	}

	return b.String()
}

// hasAccessGrant reports whether this invite left the person inside the
// Cloudflare Access group, which is what makes the App Launcher useful to them.
//
// "Skipped" counts: it means they already had the grant, so the launcher works.
// A *failed* cloudflare task deliberately does not — they can't sign in yet, and
// linking a page that will reject them reads as a broken invite.
func hasAccessGrant(succeeded, skipped []ServiceOutcome) bool {
	for _, group := range [][]ServiceOutcome{succeeded, skipped} {
		for _, o := range group {
			if o.ServiceKey == accessServiceKey {
				return true
			}
		}
	}
	return false
}

// marker returns the service's emoji, or a bullet fallback when it has none.
func marker(icon string) string {
	if icon == "" {
		return "▸"
	}
	return icon
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
