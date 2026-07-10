package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

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
		name    = fs.String("name", "", "person's display name (required)")
		email   = fs.String("email", "", "person's email (required for SSO + email delivery)")
		to      = fs.String("to", "", "comma-separated services, e.g. switchyard,cloudflare (required)")
		role    = fs.String("role", "member", "permission hint: member | admin")
		deliver = fs.String("deliver", "copypaste", "delivery method: copypaste | email")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: purser invite --name NAME --email EMAIL --to svc1,svc2 [--role member|admin] [--deliver copypaste|email]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	services := splitServices(*to)
	if *name == "" || len(services) == 0 {
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
		Name:     *name,
		Email:    *email,
		Services: services,
		Role:     *role,
		Delivery: model.DeliveryMethod(*deliver),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(1)
	}

	printResult(res)
}

func splitServices(csv string) []string {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// printResult writes a human summary to stderr and the credential block to
// stdout, so `purser invite … | pbcopy` (or piping to a file) captures exactly
// the copy-pasteable block.
func printResult(res *invite.Result) {
	fmt.Fprintf(os.Stderr, "\ninvite %s for %s (delivery=%s)\n", res.InviteID, res.Person.Name, res.Delivery)
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
