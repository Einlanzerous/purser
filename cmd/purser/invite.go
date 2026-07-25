package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/invite"
	"github.com/Einlanzerous/purser/internal/model"
)

// runInvite is the `purser invite` subcommand: provision one person into one or
// more services and print (or email) the credential block.
//
//	purser invite --name "Ada Lovelace" --email ada@example.com \
//	    --to switchyard,cloudflare --role member --deliver copypaste
func runInvite(args []string) {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	var (
		name     = fs.String("name", "", "person's display name (required)")
		email    = fs.String("email", "", "person's email (required for SSO + email delivery)")
		to       = fs.String("to", "", "comma-separated services, e.g. switchyard,cloudflare")
		bundle   = fs.String("bundle", "", "named onboarding bundle to grant, e.g. media | all (see PURSER_BUNDLE_*)")
		role     = fs.String("role", "member", "preset: member | admin (shortcut for --instance-role + --scopes)")
		instRole = fs.String("instance-role", "", "Switchyard instance role: member | owner (overrides --role)")
		scopes   = fs.String("scopes", "", "explicit token scopes, comma-separated (overrides --role's default)")
		projects = fs.String("projects", "", "project memberships, e.g. '*:viewer,IDEA:editor' ('*' = all projects)")
		deliver  = fs.String("deliver", "copypaste", "delivery method: copypaste | email")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: purser invite --name NAME --email EMAIL [--to svc1,svc2] [--bundle NAME]")
		fmt.Fprintln(os.Stderr, "       [--role member|admin] [--instance-role member|owner] [--scopes a,b,c]")
		fmt.Fprintln(os.Stderr, "       [--projects '*:viewer,IDEA:editor'] [--deliver copypaste|email]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "With neither --to nor --bundle, the default bundle is granted")
		fmt.Fprintln(os.Stderr, "(PURSER_DEFAULT_BUNDLE). Passing both takes the union.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	services := splitServices(*to)
	// --to and --bundle are both optional now: an invite with neither falls back
	// to the default bundle, which is the common "welcome to the family" path.
	if *name == "" {
		fs.Usage()
		os.Exit(2)
	}

	ctx := context.Background()
	a, err := setup(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(1)
	}
	defer a.cleanup()

	res, err := a.svc.Run(ctx, invite.Request{
		Name:         *name,
		Email:        *email,
		Services:     services,
		Bundle:       *bundle,
		Role:         *role,
		InstanceRole: *instRole,
		Scopes:       splitCSV(*scopes),
		Projects:     parseProjects(*projects),
		Delivery:     model.DeliveryMethod(*deliver),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(1)
	}

	printResult(res)
}

func splitServices(csv string) []string { return splitCSV(csv) }

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseProjects parses "*:viewer,IDEA:editor" into project grants, sharing the
// parser with bundle definitions so a spec means the same thing in both places.
// A malformed entry is fatal rather than skipped: silently dropping it would
// provision the person at the wrong access level, which is worse than a retry.
func parseProjects(s string) []connector.ProjectGrant {
	grants, err := invite.ParseProjectGrants(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: --projects: %v\n", err)
		os.Exit(2)
	}
	return grants
}

// printResult writes a human summary to stderr and the credential block to
// stdout, so `purser invite … | pbcopy` (or piping to a file) captures exactly
// the copy-pasteable block.
func printResult(res *invite.Result) {
	bundleNote := ""
	if res.Bundle != "" {
		bundleNote = fmt.Sprintf(" bundle=%s", res.Bundle)
	}
	fmt.Fprintf(os.Stderr, "\ninvite %s for %s (delivery=%s%s)\n", res.InviteID, res.Person.Name, res.Delivery, bundleNote)
	for _, o := range res.Outcomes {
		mark := statusMark(o)
		fmt.Fprintf(os.Stderr, "  %s %-24s %s", mark, o.DisplayName, o.Status)
		if o.Error != "" {
			fmt.Fprintf(os.Stderr, " — %s", o.Error)
		}
		fmt.Fprintln(os.Stderr)
	}

	if res.Delivery == model.DeliverEmail {
		if res.Delivered {
			fmt.Fprintf(os.Stderr, "\nCredential block emailed to %s.\n", res.Person.Email)
		}
		return
	}

	fmt.Fprintln(os.Stderr, "\n--- credential block (stdout) ---")
	fmt.Println(res.CredentialBlock)
}

func statusMark(o invite.ServiceOutcome) string {
	switch o.Status {
	case model.TaskSucceeded:
		return "✓"
	case model.TaskSkipped:
		return "•"
	default:
		if o.Pending {
			return "…"
		}
		return "✗"
	}
}
