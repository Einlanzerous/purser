package invite

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Einlanzerous/purser/internal/model"
)

// AccessServiceKey is the connector whose grant puts a person into Cloudflare's
// App Launcher. Named here as a string rather than imported from the connector
// package: the orchestrator deals in service keys throughout (bundles are plain
// key lists), and depending on one concrete connector would undo the point of
// the interface.
//
// Exported so the wiring can be pinned to the connector's own Key() in a test —
// otherwise renaming that key would silently switch the launcher off and leave
// the suite green.
const AccessServiceKey = "cloudflare"

// RenderCredentialBlock builds the copy-pasteable message an operator hands to
// the invited person (or that Purser emails them). It leads with the launcher —
// the one page listing every app they can reach — and then gives the per-service
// login URL, any one-time secret, and how to sign in.
//
// Everything here is addressed to the recipient and to nobody else. What went
// wrong is the operator's business and lives in RenderOperatorNote, which is
// never emailed — see the comment there for why the two are separate strings.
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

	// Only the two statuses that mean "the person can use this" are collected.
	// Everything else is absent from both slices by construction, which is what
	// lets the launcher gate below ask its question positively.
	var (
		succeeded []ServiceOutcome
		skipped   []ServiceOutcome
	)
	for _, o := range outcomes {
		switch o.Status {
		case model.TaskSucceeded:
			succeeded = append(succeeded, o)
		case model.TaskSkipped:
			skipped = append(skipped, o)
		}
	}

	launcher = strings.TrimSpace(launcher)

	// The launcher leads only when this invite left the person able to actually
	// use it: they're in the Access group, and every service in the invite came
	// through.
	//
	// allGranted is the important half. A half-provisioned invite is exactly
	// where a confident "start here" is worst: Access admits them to the edge, the
	// app whose provisioning failed then refuses them, and they have no way to
	// self-serve. That's the state the both-halves-or-neither invariant exists to
	// prevent, so the block must not present it as a finished welcome.
	//
	// It counts what *did* provision rather than enumerating what didn't, and
	// that direction is deliberate. An unavailable service closes the gate for the
	// same reason a failed one does — the door it stands behind is just as shut to
	// the person reading this — but it closes because it isn't a grant, not
	// because it appears on a list of bad statuses. A list would have to be
	// re-audited every time TaskStatus gains a member, and the failure mode of
	// forgetting is silent: the gate re-opens on the half-open invite it exists to
	// catch. Two statuses mean "granted", and they are the ones spelled out.
	allGranted := len(succeeded)+len(skipped) == len(outcomes)
	showLauncher := launcher != "" && allGranted && hasAccessGrant(succeeded, skipped)

	if showLauncher {
		// With the launcher leading, a standalone Cloudflare entry would repeat
		// the same email-OTP instruction under a second heading. It carries no
		// URL and no secret of its own, so the header fully replaces it.
		//
		// Suppressed before the heading is written, not after, because the
		// heading's own relevance depends on what survives.
		succeeded = withoutAccessEntry(succeeded)
		skipped = withoutAccessEntry(skipped)

		// person.Email is necessarily non-empty here: the Cloudflare connector
		// refuses to provision without one, so an Access grant implies an email.
		fmt.Fprintf(&b, "Hi %s — you've been granted access to the Construct.\n\n", greeting)
		fmt.Fprintf(&b, "🚀 Start here: %s\n", launcher)
		fmt.Fprintf(&b, "    Sign in with the email one-time-PIN sent to %s — no password.\n", person.Email)
		b.WriteString("    The Construct apps behind single sign-on are listed there.\n")

		// A launcher-only invite — Cloudflare and nothing else — has no per-app
		// details left once its own entry is dropped, and the heading would
		// announce a section that never arrives.
		if len(succeeded)+len(skipped) > 0 {
			b.WriteString("\nPer-app details, including anything the launcher can't sign you into:\n")
		}
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

	// Warn about secrets only when the block actually carries one. An SSO-only
	// invite shows no secret anywhere, and warning about "the secrets above"
	// sends the reader hunting for something that was never there.
	if hasSecret(succeeded) {
		b.WriteString("\nKeep any secrets above private — they are shown once and cannot be retrieved later.\n")
	}

	return b.String()
}

// hasSecret reports whether any rendered entry carried one-time material. It
// takes the post-suppression list, so a dropped entry can't justify the warning.
func hasSecret(succeeded []ServiceOutcome) bool {
	for _, o := range succeeded {
		if o.Secret != "" {
			return true
		}
	}
	return false
}

// RenderOperatorNote lists the services this invite could not provision, for
// whoever ran it. It returns "" when every service came through.
//
// This is a separate string from the credential block rather than a trailing
// section of it, and that separation is the whole point. The two were one string
// once, headed "(Operator note — not for the recipient)" — and `--deliver email`
// mails that string verbatim, so a single failed connector sent an external
// invitee a failure list that announced it wasn't for them, carrying raw
// `err.Error()` text: status codes, upstream bodies, whatever a connector chose
// to put there (PRSR-19). Splitting at the source leaves the emailer nothing to
// filter and no way to get the audience wrong.
//
// Failures and unavailable connectors are listed under separate headings because
// they ask the operator for different things: a failure wants investigating and
// a retry, while an unavailable connector wants a token set and will report the
// same thing on every retry until someone does. Listing them together under
// "what failed" — which is what a `(pending)` suffix on a failure line amounted
// to — puts the one item nobody needs to act on today at the top of the list of
// things to act on (PRSR-21).
//
// Neither section is dropped as uninteresting. Unavailable is not always "a
// token you already know you didn't set": Cloudflare returns ErrPending carrying
// the exact dashboard step to perform by hand, which is the operator's whole
// instruction for finishing that invite.
func RenderOperatorNote(outcomes []ServiceOutcome) string {
	var failed, unavailable strings.Builder
	for _, o := range outcomes {
		switch o.Status {
		case model.TaskSucceeded, model.TaskSkipped:
			// The person got it. Nothing here is the operator's problem.
		case model.TaskUnavailable:
			fmt.Fprintf(&unavailable, "    … %s: %s\n", o.DisplayName, o.Error)
		default:
			// Failed, plus anything a later change adds to TaskStatus. Reported
			// rather than dropped, and for the same reason the launcher gate above
			// enumerates the good statuses: this note is the operator's only
			// account of what didn't provision, so a status that falls through
			// every arm silently reads as "everything worked".
			fmt.Fprintf(&failed, "    ✗ %s: %s\n", o.DisplayName, o.Error)
		}
	}
	if failed.Len() == 0 && unavailable.Len() == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("Operator note — not for the recipient:\n")
	if failed.Len() > 0 {
		b.WriteString("\n  Failed — worth a retry once fixed:\n")
		b.WriteString(failed.String())
	}
	if unavailable.Len() > 0 {
		b.WriteString("\n  Not available — a retry changes nothing until these are configured:\n")
		b.WriteString(unavailable.String())
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
			if o.ServiceKey == AccessServiceKey {
				return true
			}
		}
	}
	return false
}

// withoutAccessEntry drops the Cloudflare outcome, whose whole content is the
// sign-in instruction the launcher header already carries. It copies rather than
// filtering in place so the caller's Outcomes slice is left alone.
func withoutAccessEntry(outcomes []ServiceOutcome) []ServiceOutcome {
	out := make([]ServiceOutcome, 0, len(outcomes))
	for _, o := range outcomes {
		if o.ServiceKey != AccessServiceKey {
			out = append(out, o)
		}
	}
	return out
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
