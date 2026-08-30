package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/Einlanzerous/purser/internal/spinup"
)

const teardownUsage = "usage: purser teardown-service --service KEY --hostname HOST [--apply]"

// runTeardownService is `purser teardown-service`: take down the edge Purser
// stood up for a service — its DNS record, its Cloudflare Access application,
// and (when it was tunnelled) its ingress route (PRSR-34, epic PRSR-22).
//
//	# what would go, and nothing else: no upstream call is made at all
//	purser teardown-service --service interlock \
//	  --hostname interlock.zerogravity.industries
//
//	# and then remove it
//	purser teardown-service --service interlock \
//	  --hostname interlock.zerogravity.industries --apply
//
// It takes both keys, and that is the answer to the question this command
// existed on a ticket for a fortnight to settle: a service_resource row proves
// Purser created something at a hostname and does not prove the hostname is
// still that service's to remove. Purser cannot tell from the outside, so it
// asks the operator to say who owns it and refuses the whole run — removing
// nothing — when its own records disagree. `offboard` has the same shape for the
// same reason: one target, named twice over, and no bulk mode.
//
// It previews by default and acts only under --apply, which every other
// destructive path here also does. The default matters more on this one: Ensure
// previews because one of its three steps is a read-modify-write of shared
// state, and here every step is a deletion. Like `offboard`, and unlike
// `provision-service`, a dry run makes no upstream call whatsoever — it has
// nothing to ask, because the records are the plan.
func runTeardownService(args []string) {
	fs := flag.NewFlagSet("teardown-service", flag.ExitOnError)
	var (
		key      = fs.String("service", "", "service key the hostname belongs to, e.g. argosy (required — checked against Purser's records before anything is removed)")
		hostname = fs.String("hostname", "", "the hostname to take down (required)")
		apply    = fs.Bool("apply", false, "actually delete; without it this is a plan and nothing upstream is contacted")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, teardownUsage)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Removes what `purser provision-service` recorded for a hostname: the DNS")
		fmt.Fprintln(os.Stderr, "record first, so the name stops resolving, then the ingress route and the")
		fmt.Fprintln(os.Stderr, "Access application. Only ids Purser recorded are targeted — anything at")
		fmt.Fprintln(os.Stderr, "the hostname that Purser did not create is left alone.")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Plans by default, and a plan contacts nothing. --apply to act.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	requireNoOperands(fs, teardownUsage)

	// Checked before the database is opened, so a missing flag is a usage error
	// rather than a connection followed by one. The orchestrator refuses these
	// too — it is its own precondition and not something a surface may skip —
	// and this is what decides the exit code and prints the usage line.
	if *key == "" || *hostname == "" {
		fmt.Fprintln(os.Stderr, "purser: --service and --hostname are both required; the service key is checked against Purser's records before anything is removed")
		fmt.Fprintln(os.Stderr, teardownUsage)
		os.Exit(2)
	}

	ctx := context.Background()
	a, err := setup(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(1)
	}

	// os.Exit skips deferred calls and this command has non-zero exits that are
	// not crashes, so the pool is closed explicitly — the arrangement offboard
	// and provision-service both use.
	code := teardownService(ctx, a, spinup.TeardownRequest{
		ServiceKey: *key,
		Hostname:   *hostname,
		Apply:      *apply,
	})
	a.cleanup()
	os.Exit(code)
}

// teardownService runs the request and prints it, returning the process exit
// code. Split out so the caller can close the pool before exiting.
func teardownService(ctx context.Context, a *app, req spinup.TeardownRequest) int {
	res, err := a.spin.Teardown(ctx, req)
	if err != nil {
		// Teardown errors only for a request that cannot be attempted: a missing
		// or malformed identifier, a failed read of Purser's own records, or the
		// hostname being recorded to another service. Anything a provisioner did
		// or failed to do is a finding, so nothing partial hides behind this.
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		return 2
	}
	printTeardown(res)
	return teardownExit(res)
}

// printTeardown writes the per-resource plan to stdout and the operator's
// summary to stderr — the split audit, offboard and provision-service all use:
// the table is the answer, the rest is commentary on it.
func printTeardown(res *spinup.TeardownResult) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RESOURCE\tACTION\tDETAIL")
	for _, f := range res.Findings {
		action := string(f.Status)
		if f.Applied {
			action += " ✓"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.DisplayName, action, orDash(teardownDetail(f)))
	}
	_ = w.Flush()

	c := res.Counts()
	fmt.Fprintf(os.Stderr, "\n%s (%s): %d to remove, %d already gone, %d never recorded\n",
		res.ServiceKey, res.Hostname,
		res.Pending(), c[spinup.TeardownGone], c[spinup.TeardownNone])

	// One line each, in the order they want acting on: what Purser cannot do,
	// then what upstream will not permit, then what the ordering held back, then
	// what merely needs another go.
	if n := c[spinup.TeardownUnavailable]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d unavailable — Purser is not configured to remove %s, and %s still there.\n",
			n, plural2(n, "that resource", "those resources"), plural2(n, "it is", "they are"))
	}
	if n := c[spinup.TeardownRefused]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d refused — upstream is in a state Purser will not act on. Re-running repeats this until it is fixed there.\n", n)
	}
	if n := c[spinup.TeardownBlocked]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d held back so nothing is taken away in front of a hostname that still resolves.\n", n)
	}
	if n := c[spinup.TeardownRemovedNotRecorded]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d removed upstream but NOT recorded — %s gone, and Purser's rows still say otherwise. Fix the rows: a later spin-up would read them as something to adopt.\n",
			n, plural2(n, "it is", "they are"))
	}
	if n := c[spinup.TeardownFailed]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d failed — nothing was recorded as removed, so a re-run retries %s.\n",
			n, plural2(n, "it", "them"))
	}

	fmt.Fprintf(os.Stderr, "\n%s\n", teardownOutcome(res))

	// Warnings before the errors, and never repeated in the table: a removal
	// that succeeded while possibly costing something *else* is easy to skim
	// past, because its own line says the step worked.
	for _, f := range res.Findings {
		if f.Warning != "" {
			fmt.Fprintf(os.Stderr, "\n! %s: %s\n", f.DisplayName, f.Warning)
		}
	}

	// The errors go last and in full, for the reason provision-service prints
	// them there: they are the longest lines here, and the DETAIL column would
	// truncate the useful half of a refusal.
	for _, f := range res.Findings {
		if f.Err != "" {
			fmt.Fprintf(os.Stderr, "\n%s (%s):\n  %s\n", f.DisplayName, f.Status, f.Err)
		}
	}
}

// teardownDetail is the DETAIL cell. A resource carrying an Err has its full
// text printed below the table, so the cell says only that there is one.
func teardownDetail(f spinup.TeardownFinding) string {
	if f.Err != "" && f.Detail == "" {
		return "see below"
	}
	return f.Detail
}

// teardownOutcome is the closing line — the one an operator reads if they read
// nothing else.
//
// "Nothing pending" is not "the hostname is clear", for the reason it isn't on
// the way up either: Pending counts only what --apply would act on, so a plan
// against an unconfigured deployment reports zero and means nothing of the sort.
// The verdict comes from NeedsAttention, which is also what sets the exit code.
func teardownOutcome(res *spinup.TeardownResult) string {
	c := res.Counts()
	switch {
	case res.Applied:
		return fmt.Sprintf("Removed %d of %d.", res.Changed(), len(res.Findings))
	case res.Pending() > 0:
		return fmt.Sprintf("Plan — nothing removed, and nothing upstream was contacted. Re-run with --apply to remove %d resource%s.",
			res.Pending(), plural(res.Pending()))
	case teardownExit(res) != 0:
		return "Plan — there is nothing --apply would remove. The resources above need attention first."
	case c[spinup.TeardownNone] == len(res.Findings):
		// Every kind reports "none", which is the shape a typo'd hostname makes
		// as well as a hostname that was genuinely never stood up. Worth saying
		// outright, because the alternative wording — "nothing to do" — is the
		// most reassuring line this command can print and it would be printed
		// over a hostname nobody has ever heard of.
		return "Plan — Purser has no record of anything at this hostname, so it has nothing to remove. Check the spelling if you expected otherwise."
	default:
		return "Plan — nothing to do; everything Purser recorded here is already gone."
	}
}

// teardownExit reports whether the hostname's recorded edge is gone.
//
// Non-zero for every status that leaves it otherwise, `unavailable` included —
// which follows offboard rather than invite, and for its reason. On an invite,
// unavailable means nothing was granted and waiting harms nobody. On a removal
// it means a resource is still live while the operator has been told the service
// is being taken down.
//
// A *plan* with removals outstanding still exits 0: previewing succeeded at
// previewing, and the pending count is what says there is more to do.
func teardownExit(res *spinup.TeardownResult) int {
	if len(res.NeedsAttention()) > 0 {
		return 1
	}
	return 0
}
