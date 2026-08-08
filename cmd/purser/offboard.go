package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/Einlanzerous/purser/internal/invite"
)

const offboardUsage = "usage: purser offboard --email EMAIL [--to svc1,svc2] [--apply]"

// runOffboard is `purser offboard`: revoke a person's access across the services
// they hold (PRSR-17). The opposite of `invite`, and deliberately shaped as its
// mirror image rather than as a flag on it.
//
//	purser offboard --email ada@example.com            # preview; writes nothing
//	purser offboard --email ada@example.com --apply    # actually revoke
//	purser offboard --email ada@… --to argosy --apply  # one service only
//
// Dry run is the default because this is the one command whose mistake a re-run
// does not fix. `invite` defaults to acting; this defaults to reporting.
func runOffboard(args []string) {
	fs := flag.NewFlagSet("offboard", flag.ExitOnError)
	var (
		email = fs.String("email", "", "the person's email (required — the identity key)")
		to    = fs.String("to", "", "limit to these services, comma-separated (default: everything they hold)")
		apply = fs.Bool("apply", false, "actually revoke; without it this is a preview")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, offboardUsage)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Revokes access — it does not delete people. Purser's account rows are")
		fmt.Fprintln(os.Stderr, "marked deprovisioned and kept, so the record of what someone held")
		fmt.Fprintln(os.Stderr, "survives. Previews by default; --apply to act.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	requireNoOperands(fs, offboardUsage)

	wanted, err := invite.NormalizeEmail(*email)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		fmt.Fprintln(os.Stderr, offboardUsage)
		os.Exit(2)
	}

	ctx := context.Background()
	a, err := setup(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(1)
	}
	defer a.cleanup()

	res, err := a.svc.Offboard(ctx, invite.OffboardRequest{
		Email:    wanted,
		Services: splitCSV(*to),
		Apply:    *apply,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		printAddPersonHint(err, wanted)
		if errors.Is(err, invite.ErrUnknownService) {
			os.Exit(2)
		}
		os.Exit(1)
	}

	printOffboard(res)
	os.Exit(offboardExit(res))
}

// printOffboard writes the per-service verdicts to stdout and the operator's
// guidance to stderr, matching how audit splits the two.
func printOffboard(res *invite.OffboardResult) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tUSERNAME\tACTION")
	for _, f := range res.Findings {
		action := string(f.Action)
		if f.Applied {
			action += " ✓"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.ServiceKey, orDash(f.Username), action)
	}
	if len(res.Findings) == 0 {
		fmt.Fprintln(w, "(no accounts recorded — nothing to revoke)")
	}
	_ = w.Flush()

	c := res.Counts()
	fmt.Fprintf(os.Stderr, "\n%s <%s>: %d to revoke, %d nothing to do, %d unavailable, %d failed\n",
		res.Person.Name, res.Person.Email,
		c[invite.ActionRevoke], c[invite.ActionNothingToDo],
		c[invite.ActionUnavailable], c[invite.ActionFailed])

	if !res.Applied {
		if c[invite.ActionRevoke] > 0 {
			fmt.Fprintf(os.Stderr, "\nPreview — nothing revoked. Re-run with --apply to revoke %d service%s.\n",
				c[invite.ActionRevoke], plural(c[invite.ActionRevoke]))
		} else {
			fmt.Fprintln(os.Stderr, "\nPreview — nothing to revoke.")
		}
	} else {
		fmt.Fprintf(os.Stderr, "\nRevoked %d of %d.\n", res.Revoked(), len(res.Findings))
	}

	if note := invite.RenderOffboardNote(res); note != "" {
		fmt.Fprintf(os.Stderr, "\n%s", note)
	}

	// The half-done case looks finished, so say it outright: revoking Switchyard
	// tokens does not close its SSO door — the Cloudflare Access group does.
	if leavesSSOOpen(res) {
		fmt.Fprintln(os.Stderr, "\nNote: Switchyard was revoked but Cloudflare Access was not.")
		fmt.Fprintln(os.Stderr, "Revoking tokens removes API access; the SSO login is gated by the Access")
		fmt.Fprintln(os.Stderr, "group, so they can still sign in. Include --to cloudflare to close it.")
	}
}

// leavesSSOOpen reports the specific partial offboard that reads as complete and
// isn't: Switchyard revoked while the Cloudflare Access grant survives.
func leavesSSOOpen(res *invite.OffboardResult) bool {
	if !res.Applied {
		return false
	}
	var switchyardRevoked, cloudflareStands bool
	for _, f := range res.Findings {
		switch f.ServiceKey {
		case "switchyard":
			switchyardRevoked = f.Applied
		case "cloudflare":
			cloudflareStands = !f.Applied && f.Action != invite.ActionNothingToDo
		}
	}
	// An unscoped run reports on cloudflare too, so "not mentioned at all" means
	// the operator scoped it out — which is exactly the case worth flagging.
	mentioned := false
	for _, f := range res.Findings {
		if f.ServiceKey == "cloudflare" {
			mentioned = true
		}
	}
	return switchyardRevoked && (cloudflareStands || !mentioned)
}

// offboardExit reports whether access is actually gone.
//
// A failed revoke exits non-zero so a script can tell; an *unavailable* one does
// too, because from the offboarding point of view they are the same outcome —
// the person still has access. That is the opposite of the invite path, where
// unavailable is benign, and it is deliberate: there, nothing was granted and
// nobody is harmed by waiting; here, something was meant to be taken away and
// wasn't.
func offboardExit(res *invite.OffboardResult) int {
	c := res.Counts()
	if c[invite.ActionFailed] > 0 || c[invite.ActionUnavailable] > 0 {
		return 1
	}
	return 0
}
