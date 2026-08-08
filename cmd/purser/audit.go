package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/Einlanzerous/purser/internal/invite"
)

// runAudit is `purser audit` (report only) and `purser reconcile` (applies the
// repairs). Both walk the same code path — the only difference is the Apply
// flag — so what you see in the report is exactly what reconcile will do.
//
//	purser audit                          # whole roster, no writes
//	purser audit --email ada@example.com  # one person
//	purser reconcile --email ada@…        # write the records
func runAudit(args []string, apply bool) {
	name := "audit"
	if apply {
		name = "reconcile"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var (
		email = fs.String("email", "", "limit to one person (default: everyone)")
		to    = fs.String("to", "", "limit to these services, comma-separated (default: all)")
		all   = fs.Bool("all", false, "required to reconcile the entire roster at once")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: purser %s [--email EMAIL] [--to svc1,svc2]", name)
		if apply {
			fmt.Fprint(os.Stderr, " [--all]")
		}
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "")
		if apply {
			fmt.Fprintln(os.Stderr, "Writes records to match upstream reality. Never provisions and never")
			fmt.Fprintln(os.Stderr, "mints a credential — run `purser audit` first to preview.")
		} else {
			fmt.Fprintln(os.Stderr, "Compares Purser's records against upstream. Read-only.")
		}
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	// Reconciling everyone is a bulk write; make it deliberate rather than the
	// default that a bare `purser reconcile` falls into.
	if apply && strings.TrimSpace(*email) == "" && !*all {
		fmt.Fprintln(os.Stderr, "purser: refusing to reconcile the whole roster without --all (or use --email)")
		os.Exit(2)
	}

	ctx := context.Background()
	a, err := setup(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(1)
	}
	defer a.cleanup()

	res, err := a.svc.Audit(ctx, invite.AuditRequest{
		Email:    *email,
		Services: splitCSV(*to),
		Apply:    apply,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		// An address nobody holds is a roster gap with a command that fixes it,
		// and a mistyped service is a flag to retype — neither is an outage, so
		// they read the same here as they do from `person show` / `person list`.
		printAddPersonHint(err, strings.ToLower(strings.TrimSpace(*email)))
		if errors.Is(err, invite.ErrUnknownService) {
			os.Exit(2)
		}
		os.Exit(1)
	}
	printAudit(res)
}

// printAudit renders the findings as a table, hiding the (usually large) set of
// rows where records already match reality.
func printAudit(res *invite.AuditResult) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PERSON\tSERVICE\tRECORDED\tUPSTREAM\tACTION")

	shown := 0
	for _, f := range res.Findings {
		if f.Action == invite.ActionNone || f.Action == invite.ActionAbsent {
			continue // nothing to say about rows that already agree
		}
		shown++
		who := f.Person.Email
		if who == "" {
			who = f.Person.Name
		}
		action := string(f.Action)
		if f.Applied {
			action += " ✓"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", who, f.ServiceKey, yesNo(f.Recorded), f.Upstream, action)
	}
	if shown == 0 {
		fmt.Fprintln(w, "(no drift — every record matches upstream)")
	}
	_ = w.Flush()

	c := res.Counts()
	fmt.Fprintf(os.Stderr, "\n%d checked: %d ok, %d to record, %d stale, %d unverifiable, %d errors\n",
		len(res.Findings), c[invite.ActionNone]+c[invite.ActionAbsent],
		c[invite.ActionRecord], c[invite.ActionMarkStale],
		c[invite.ActionSkipped], c[invite.ActionError])

	if !res.Applied && (c[invite.ActionRecord] > 0 || c[invite.ActionMarkStale] > 0) {
		fmt.Fprintln(os.Stderr, "\nDry run — nothing written. Re-run as `purser reconcile` to apply.")
	}
	if c[invite.ActionSkipped] > 0 {
		fmt.Fprintln(os.Stderr, "Unverifiable services have no upstream lookup endpoint; they are left untouched.")
	}
	// Surface the reasons once rather than per row — they repeat heavily.
	seen := map[string]bool{}
	for _, f := range res.Findings {
		if (f.Action == invite.ActionSkipped || f.Action == invite.ActionError) && f.Err != "" && !seen[f.Err] {
			seen[f.Err] = true
			fmt.Fprintf(os.Stderr, "  %s: %s\n", f.ServiceKey, f.Err)
		}
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
