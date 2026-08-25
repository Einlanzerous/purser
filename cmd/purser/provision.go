package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/Einlanzerous/purser/internal/spinup"
)

const provisionUsage = "usage: purser provision-service --service KEY --hostname HOST --mode tunnelled|direct --upstream UPSTREAM --access gated|bookmark|none [--tunnel prod] [--logo URL] [--apply]"

// runProvisionService is `purser provision-service`: stand up the edge for a
// service — its DNS record, its Cloudflare Access application, and (when it is
// tunnelled) its ingress route (PRSR-31, epic PRSR-22).
//
//	# preview Argosy, which is already up: three no-ops and nothing written
//	purser provision-service --service argosy \
//	  --hostname argosy.zerogravity.industries \
//	  --mode direct --upstream 100.64.0.7 --access bookmark
//
//	# a tunnelled, gated service
//	purser provision-service --service interlock \
//	  --hostname interlock.zerogravity.industries \
//	  --mode tunnelled --tunnel prod --upstream http://interlock:8080 \
//	  --access gated --apply
//
// It previews by default and acts only under --apply, the same way `offboard`
// does and the opposite of `invite`. The reasoning is settled in
// internal/spinup/ensure.go rather than here so the three provisioners cannot
// disagree about it: two of the three steps are additive and idempotent, but the
// third appends to a document holding every other service's routes on that
// tunnel, and that is the mistake re-running does not fix.
//
// The spec is flags rather than a file. Config here is env-only by house
// convention, and a spec is not configuration — it is an argument, written
// rarely and read carefully, which is the same reason a tunnelled spec has to
// name its tunnel instead of defaulting to one.
func runProvisionService(args []string) {
	fs := flag.NewFlagSet("provision-service", flag.ExitOnError)
	var (
		key      = fs.String("service", "", "service key, e.g. argosy (required — labels the resource rows)")
		name     = fs.String("name", "", "display name for the Access application (default: the service key)")
		hostname = fs.String("hostname", "", "public hostname, e.g. argosy.zerogravity.industries (required)")
		mode     = fs.String("mode", "", "how traffic reaches it: tunnelled or direct (required)")
		upstream = fs.String("upstream", "", "tunnelled: the origin url cloudflared forwards to (http://argosy:8096); direct: the record's value, an ip or hostname (required)")
		access   = fs.String("access", "", "Access surface: gated, bookmark or none (required)")
		logo     = fs.String("logo", "", "https url for the launcher tile's icon")
		tunnel   = fs.String("tunnel", "", "which tunnel carries it: prod (required for --mode tunnelled, refused for direct)")
		apply    = fs.Bool("apply", false, "actually create and update; without it this is a plan")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, provisionUsage)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Stands up a service's edge: DNS record, Cloudflare Access application,")
		fmt.Fprintln(os.Stderr, "and — for a tunnelled service — its ingress route. Idempotent per")
		fmt.Fprintln(os.Stderr, "(hostname, kind): an edge that already matches the spec is adopted, not")
		fmt.Fprintln(os.Stderr, "rebuilt. Plans by default; --apply to act.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	requireNoOperands(fs, provisionUsage)

	spec := spinup.ServiceSpec{
		Key:         *key,
		DisplayName: *name,
		Hostname:    *hostname,
		Mode:        spinup.Mode(*mode),
		Upstream:    *upstream,
		Access:      spinup.AccessShape(*access),
		LogoURL:     *logo,
		Tunnel:      spinup.TunnelRef(*tunnel),
	}
	// Validated before the database is touched. Everything the spec can be wrong
	// about is cheaper to catch here than after a connection, and one of them —
	// a hostname from another zone — is cheaper to catch here than anywhere
	// upstream, where Cloudflare would silently append the zone to it.
	if _, err := spec.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		fmt.Fprintln(os.Stderr, provisionUsage)
		os.Exit(2)
	}

	ctx := context.Background()
	a, err := setup(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(1)
	}

	// os.Exit skips deferred calls and this command has non-zero exits that are
	// not crashes, so the pool is closed explicitly rather than by a defer only
	// the error paths would reach — the arrangement offboard uses.
	code := provisionService(ctx, a, spinup.Request{Spec: spec, Apply: *apply})
	a.cleanup()
	os.Exit(code)
}

// provisionService runs the request and prints it, returning the process exit
// code. Split out so the caller can close the pool before exiting.
func provisionService(ctx context.Context, a *app, req spinup.Request) int {
	res, err := a.spin.Ensure(ctx, req)
	if err != nil {
		// Ensure errors only for a request that cannot be attempted at all — an
		// invalid spec, an unresolvable tunnel ref, or a failed read of Purser's
		// own records. Anything a provisioner did or failed to do is a finding,
		// so there is no partial-run case hiding behind this branch.
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		return 2
	}
	printProvision(res)
	return provisionExit(res)
}

// printProvision writes the per-resource plan to stdout and the operator's
// summary to stderr, the split audit and offboard both use: the table is the
// answer, the rest is commentary on it.
func printProvision(res *spinup.Result) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "RESOURCE\tACTION\tDETAIL")
	for _, f := range res.Findings {
		action := string(f.Status)
		if f.Applied {
			action += " ✓"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.DisplayName, action, orDash(provisionDetail(f)))
	}
	_ = w.Flush()

	c := res.Counts()
	fmt.Fprintf(os.Stderr, "\n%s (%s): %d in place, %d to do, %d skipped\n",
		res.Spec.Key, res.Spec.Hostname,
		c[spinup.StepOK], res.Pending(), c[spinup.StepSkipped])

	// Each of these wants a different thing from the operator, so each gets its
	// own line rather than a shared "problems: 3". The order is the order they
	// want acting on: what Purser cannot do, then what upstream will not permit,
	// then what merely needs another go.
	if n := c[spinup.StepUnavailable]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d unavailable — Purser is not configured for %s; set the variable each line names.\n",
			n, plural2(n, "that step", "those steps"))
	}
	if n := c[spinup.StepRefused]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d refused — upstream is in a state Purser will not write to. Re-running repeats this until it is fixed there.\n", n)
	}
	if n := c[spinup.StepUnknown]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d could not be read, so nothing was decided from %s — re-run.\n",
			n, plural2(n, "it", "them"))
	}
	if n := c[spinup.StepBlocked]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d held back so the hostname is not published in front of a step that did not land.\n", n)
	}
	if n := c[spinup.StepOrphaned]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d recorded here but not called for by this spec — still live, and this spec will never remove %s.\n",
			n, plural2(n, "it", "them"))
	}
	if n := c[spinup.StepMissing]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d recorded by Purser and gone from upstream — removed outside Purser.\n", n)
	}
	if n := c[spinup.StepAppliedNotRecorded]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d changed upstream but NOT recorded — Purser cannot tear down what it does not hold an id for. Re-run to adopt %s back.\n",
			n, plural2(n, "it", "them"))
	}
	if n := c[spinup.StepFailed]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d failed — nothing was recorded for %s, so a re-run reconsiders %s from scratch.\n",
			n, plural2(n, "it", "them"), plural2(n, "it", "them"))
	}

	fmt.Fprintf(os.Stderr, "\n%s\n", provisionOutcome(res))

	// Warnings before the errors: a step that succeeded while possibly damaging
	// something *else* is easy to skim past, because its own line in the table
	// says the step worked. It gets one line here and is not repeated in the
	// table's DETAIL cell — printing it twice is how a reader learns to discount
	// the message that most needs believing when it fires.
	for _, f := range res.Findings {
		if f.Warning != "" {
			fmt.Fprintf(os.Stderr, "\n! %s: %s\n", f.DisplayName, f.Warning)
		}
	}

	// The errors go last and in full. They are the longest lines here — a
	// refused ingress document explains itself in a sentence and a half — and
	// putting them in the table's DETAIL column would either wrap or be
	// truncated, which is how the useful half of a refusal gets lost.
	for _, f := range res.Findings {
		if f.Err != "" {
			fmt.Fprintf(os.Stderr, "\n%s (%s):\n  %s\n", f.DisplayName, f.Status, f.Err)
		}
	}
}

// provisionDetail is the DETAIL cell.
//
// A step carrying an Err has its full text printed below the table, so the cell
// says only which kind of trouble it is; otherwise Detail describes what is
// upstream now, which is the line an operator checks the plan against.
func provisionDetail(f spinup.StepFinding) string {
	if f.Err != "" && f.Detail == "" {
		return "see below"
	}
	return f.Detail
}

// provisionOutcome is the closing line — the one an operator reads if they read
// nothing else.
//
// It must not say the reassuring thing when the reassuring thing is not true,
// and getting that right takes more than a pending count. Pending() deliberately
// excludes unavailable, refused, unknown and blocked, because --apply fixes none
// of them; so "nothing pending" is *not* the same claim as "the edge is as the
// spec asks". Read as though it were, this printed "the edge already matches
// this spec" over a plan whose DNS step was unavailable — a hostname that does
// not resolve at all, reported as a service that is up. Found by running it, not
// by reading it.
func provisionOutcome(res *spinup.Result) string {
	switch {
	case res.Applied:
		return fmt.Sprintf("Applied %d of %d.", res.Changed(), len(res.Findings))
	case res.Pending() > 0:
		return fmt.Sprintf("Plan — nothing created or changed. Re-run with --apply to act on %d step%s.",
			res.Pending(), plural(res.Pending()))
	case provisionExit(res) != 0:
		return "Plan — there is nothing --apply would fix. The steps above need attention first."
	default:
		return "Plan — nothing to do; the edge already matches this spec."
	}
}

// provisionExit reports whether the service's edge is as the spec asks.
//
// Non-zero for every status that leaves it otherwise, including `unavailable` —
// which follows offboard rather than invite, and for the same reason. On an
// invite, unavailable means nothing was granted and waiting harms nobody. Here
// it means a step of the edge does not exist, so the hostname does not work; the
// operator asked for a service to be up and it is not.
//
// A *plan* with work outstanding still exits 0: previewing succeeded at
// previewing, and the pending count is what says there is more to do. That is
// offboard's rule too, and it is what lets a plan be run in a pipeline without
// every un-applied step reading as a fault.
func provisionExit(res *spinup.Result) int {
	c := res.Counts()
	for _, st := range []spinup.StepStatus{
		spinup.StepFailed,
		spinup.StepAppliedNotRecorded,
		spinup.StepUnavailable,
		spinup.StepRefused,
		spinup.StepUnknown,
		spinup.StepBlocked,
	} {
		if c[st] > 0 {
			return 1
		}
	}
	return 0
}

// plural2 picks between a singular and a plural phrase. The counts above read as
// sentences rather than as labels, and "1 could not be read, so nothing was
// decided from them" is the kind of wrong that makes a reader doubt the number.
func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
