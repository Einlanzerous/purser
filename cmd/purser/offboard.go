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

	// os.Exit skips deferred calls, and this command has a non-zero success-ish
	// exit — so the pool is closed explicitly before every exit rather than by a
	// defer that only the error paths would ever reach.
	code := offboard(ctx, a, invite.OffboardRequest{
		Email:    wanted,
		Services: splitCSV(*to),
		Apply:    *apply,
	})
	a.cleanup()
	os.Exit(code)
}

// offboard runs the request and prints it, returning the process exit code.
// Split out so the caller can close the pool before exiting.
func offboard(ctx context.Context, a *app, req invite.OffboardRequest) int {
	res, err := a.svc.Offboard(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		printAddPersonHint(err, req.Email)
		if errors.Is(err, invite.ErrUnknownService) {
			return 2
		}
		return 1
	}
	printOffboard(res)
	return offboardExit(res)
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
	if n := c[invite.ActionRevokedNotRecorded]; n > 0 {
		fmt.Fprintf(os.Stderr, "%d revoked upstream but not recorded — see below.\n", n)
	}

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

	if len(res.SkippedActive) > 0 {
		fmt.Fprintf(os.Stderr, "\nStill active, excluded by --to: %s\n", strings.Join(res.SkippedActive, ", "))
	}

	// What this command knows is what Purser recorded. An empty table means "no
	// recorded access", which is not the same claim as "no access" — the roster
	// can be behind, which is the whole reason `audit` exists. Saying so is
	// cheap; asserting the stronger thing from local rows alone would be the
	// treat-unverifiable-as-absent mistake the rest of the codebase refuses.
	if len(res.Findings) == 0 {
		fmt.Fprintf(os.Stderr, "\nThis reads Purser's records only. If they were set up outside Purser,\nrun `purser audit --email %s` to check upstream first.\n", res.Person.Email)
	}

	if note := invite.RenderOffboardNote(res); note != "" {
		fmt.Fprintf(os.Stderr, "\n%s", note)
	}

	// The half-done case looks finished, so say it outright: revoking Switchyard
	// tokens does not close its SSO door — the Cloudflare Access group does.
	switch leavesSSOOpen(res) {
	case ssoClosed:
	case ssoOpenScopedOut:
		printSSOWarning(res, "Include --to cloudflare to close it.")
	case ssoOpenCantClose:
		// --to cloudflare would be a no-op here: it is already in scope and the
		// connector can't act. Prescribing it anyway teaches the operator to
		// ignore this warning, which is the one that matters.
		printSSOWarning(res, "Purser can't close it — remove them from the Access group by hand.")
	}
}

func printSSOWarning(res *invite.OffboardResult, remedy string) {
	verb := "would be revoked"
	if res.Applied {
		verb = "was revoked"
	}
	fmt.Fprintf(os.Stderr, "\nNote: Switchyard %s, but their Cloudflare Access grant stands.\n", verb)
	fmt.Fprintln(os.Stderr, "Revoking tokens removes API access; the sign-in is gated by the Access")
	fmt.Fprintf(os.Stderr, "group, so they could still log in. %s\n", remedy)
}

// ssoState is why (or whether) a Cloudflare Access grant is left standing.
type ssoState int

const (
	ssoClosed        ssoState = iota // nothing left open
	ssoOpenScopedOut                 // --to excluded it; naming it would fix this
	ssoOpenCantClose                 // in scope, but the connector can't act
)

// leavesSSOOpen reports the partial offboard that reads as complete and isn't:
// Switchyard revoked while a live Cloudflare Access grant survives.
//
// Two things this must get right, both learned the hard way. It fires on the
// *preview* as well as the apply — a warning about an irreversible step is worth
// least after the step. And it fires only when there is an active cloudflare
// account still standing: if the person has no such row, nothing is open and the
// remedy it suggests would be a provable no-op, which teaches the operator to
// ignore the warning that matters.
func leavesSSOOpen(res *invite.OffboardResult) ssoState {
	// "Closing" means the access will be gone when this run is done — already
	// applied on a real run, or slated to be on a preview. Reading Applied alone
	// would make every preview look like it left everything open.
	closing := func(f invite.OffboardFinding) bool {
		if res.Applied {
			return f.Applied
		}
		return f.Action == invite.ActionRevoke
	}

	var switchyardClosing, cloudflareStuck bool
	for _, f := range res.Findings {
		switch f.ServiceKey {
		case "switchyard":
			switchyardClosing = closing(f)
		case "cloudflare":
			// NothingToDo means there is no grant to close, so nothing is left
			// open — warning there would prescribe a provable no-op.
			cloudflareStuck = !closing(f) && f.Action != invite.ActionNothingToDo
		}
	}
	if !switchyardClosing {
		return ssoClosed
	}
	// A cloudflare grant scoped out by --to produces no finding at all, which is
	// precisely the case this warning is for. SkippedActive is what tells it
	// apart from the person simply never having had one — and it is a different
	// state from "in scope and unfixable", because only one of them has a remedy
	// the operator can type.
	for _, key := range res.SkippedActive {
		if key == "cloudflare" {
			return ssoOpenScopedOut
		}
	}
	if cloudflareStuck {
		return ssoOpenCantClose
	}
	return ssoClosed
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
	if c[invite.ActionFailed] > 0 || c[invite.ActionUnavailable] > 0 ||
		c[invite.ActionRevokedNotRecorded] > 0 {
		return 1
	}
	return 0
}
