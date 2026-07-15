package invite

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Einlanzerous/purser/internal/model"
)

// RenderCredentialBlock builds the copy-pasteable message an operator hands to
// the invited person (or that Purser emails them). It lists, per service, the
// login URL, any one-time secret, and how to sign in — including the Cloudflare
// Access email-OTP flow when a service grants SSO access.
//
// The block is plain text on purpose: it pastes cleanly into Discord, Slack,
// SMS, or an email body with no rendering surprises.
func RenderCredentialBlock(person model.Person, outcomes []ServiceOutcome) string {
	var b strings.Builder

	greeting := "there"
	if n := strings.TrimSpace(person.Name); n != "" {
		greeting = strings.Fields(n)[0]
	}
	fmt.Fprintf(&b, "Hi %s — you've been granted access to the following:\n", greeting)

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
