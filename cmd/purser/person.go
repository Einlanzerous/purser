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
	"github.com/Einlanzerous/purser/internal/model"
)

// Per-subcommand usage lines, so each flag set prints its own rather than the
// whole noun's.
const (
	personAddUsage  = "usage: purser person add --name NAME --email EMAIL [--type human|agent] [--rename] [--audit]"
	personListUsage = "usage: purser person list [--to svc1,svc2] [--type human|agent] [--all] [--json]"
	personShowUsage = "usage: purser person show --email EMAIL [--json]"
)

const personUsage = `usage: purser person <add|list|show>
  add   --name NAME --email EMAIL [--type human|agent] [--rename] [--audit]
  list  [--to svc1,svc2] [--type human|agent] [--all] [--json]
  show  --email EMAIL [--json]`

// runPerson dispatches `purser person <subcommand>`: the roster noun. `add`
// records someone and provisions nothing; `list` and `show` read the records
// back and write nothing at all (PRSR-24).
func runPerson(args []string) {
	sub := ""
	if len(args) > 0 && !isFlag(args[0]) {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "add":
		runPersonAdd(args)
	case "list":
		runPersonList(args)
	case "show":
		runPersonShow(args)
	default:
		if sub != "" {
			fmt.Fprintf(os.Stderr, "purser: unknown person subcommand %q\n", sub)
		}
		fmt.Fprintln(os.Stderr, personUsage)
		os.Exit(2)
	}
}

// runPersonAdd records that someone exists without provisioning them
// (PRSR-16). It is the entry point for people who were set up outside Purser:
// `audit` and `reconcile` walk the person table, so those people are invisible
// to the audit until a row exists.
//
//	purser person add --name "Ada Lovelace" --email ada@example.com
//	purser person add --name … --email … --audit   # and preview what they hold
func runPersonAdd(args []string) {
	fs := flag.NewFlagSet("person add", flag.ExitOnError)
	var (
		name  = fs.String("name", "", "person's display name (required)")
		email = fs.String("email", "", "person's email (required — the identity key)")
		// Empty rather than "human" so an add against an existing person keeps
		// their recorded type instead of asserting one they never claimed.
		typ    = fs.String("type", "", "person type: human | agent (default human for a new person)")
		rename = fs.Bool("rename", false, "allow changing the recorded name of an existing person")
		audit  = fs.Bool("audit", false, "after adding, run a read-only audit of what they already hold")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, personAddUsage)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Records that someone exists so the audit can see them. Writes no account")
		fmt.Fprintln(os.Stderr, "rows, calls no connectors, mints no credentials — that is `purser invite`.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	// Every input check runs before setup(), so bad flags cost an exit 2 rather
	// than a database connect, a migration run, and an exit 1. Trimmed, so
	// `--name "  "` is caught here too.
	if strings.TrimSpace(*name) == "" || strings.TrimSpace(*email) == "" {
		fs.Usage()
		os.Exit(2)
	}
	if _, err := invite.ParsePersonType(*typ); err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(2)
	}
	if _, err := invite.NormalizeEmail(*email); err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(2)
	}

	ctx := context.Background()
	a, err := setup(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(1)
	}
	defer a.cleanup()

	res, err := a.svc.AddPerson(ctx, invite.AddPersonRequest{
		Name:   *name,
		Email:  *email,
		Type:   model.PersonType(*typ),
		Rename: *rename,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		if errors.Is(err, invite.ErrNameConflict) {
			// Fixable by a flag, so it's a usage error like reconcile's --all.
			fmt.Fprintln(os.Stderr, "purser: pass --rename to change the recorded name")
			os.Exit(2)
		}
		if errors.Is(err, invite.ErrTypeConflict) {
			os.Exit(2)
		}
		os.Exit(1)
	}

	printPersonAdd(res, *audit)

	var preview *invite.AuditResult
	if *audit {
		// The same call `purser audit --email …` makes, so what it prints here
		// is exactly what reconcile would then act on. Read-only: Reconcile
		// never mutates, by contract. The header goes to stdout with the table
		// it labels, so redirecting either stream alone stays coherent.
		fmt.Printf("\n--- audit --email %s (read-only) ---\n", res.Person.Email)
		preview, err = a.svc.Audit(ctx, invite.AuditRequest{Email: res.Person.Email})
		if err != nil {
			fmt.Fprintf(os.Stderr, "purser: person recorded, but the audit preview failed: %v\n", err)
			os.Exit(1)
		}
		printAudit(preview)
	}
	printNextSteps(res.Person, preview)
}

// printPersonAdd writes the resulting row to stdout, matching audit's split:
// the result is the command's output, the guidance around it isn't.
func printPersonAdd(res *invite.AddPersonResult, previewed bool) {
	p := res.Person
	switch {
	case res.Created:
		fmt.Printf("added %s <%s> (%s)\n", p.Name, p.Email, p.Type)
	case res.Renamed:
		fmt.Printf("renamed %s <%s> (was %q)\n", p.Name, p.Email, res.PreviousName)
	default:
		fmt.Printf("already recorded: %s <%s> (%s)\n", p.Name, p.Email, p.Type)
	}
	if previewed {
		// The preview that follows does reach connectors — read-only ones — so
		// be precise about which half of the command touched the network.
		fmt.Fprintln(os.Stderr, "\nNo accounts written; the add itself called no connectors.")
		return
	}
	fmt.Fprintln(os.Stderr, "\nNo accounts written, no connectors called.")
}

// printNextSteps names the commands that turn a bare person row into records —
// the point of adding someone Purser didn't provision.
//
// preview is the audit --audit just ran, or nil. With one in hand the advice
// narrows to what it actually found: suggesting `reconcile` after a preview
// that turned up nothing to record sends the operator to a write command with
// no work to do, which teaches them to ignore the advice.
func printNextSteps(p model.Person, preview *invite.AuditResult) {
	w := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()

	fmt.Fprintln(os.Stderr, "\nNext:")
	if preview == nil {
		fmt.Fprintf(w, "  purser audit --email %s\t# what do they already hold?\n", p.Email)
		fmt.Fprintf(w, "  purser reconcile --email %s\t# record it\n", p.Email)
		fmt.Fprintf(w, "  purser invite --name %q --email %s\t# provision what they lack\n", p.Name, p.Email)
		return
	}

	c := preview.Counts()
	if n := c[invite.ActionRecord] + c[invite.ActionMarkStale]; n > 0 {
		fmt.Fprintf(w, "  purser reconcile --email %s\t# apply the %d change%s above\n",
			p.Email, n, plural(n))
	}
	fmt.Fprintf(w, "  purser invite --name %q --email %s\t# provision what they lack\n", p.Name, p.Email)
}

// printAddPersonHint names the command that fixes an address nobody holds.
//
// Shared by `person show` and `audit`, which reach the same dead end from
// opposite directions: one was asked about someone off the roster, the other
// was asked to audit them. Both answers are the same row, and `person add` is
// the only command that writes it without provisioning anything.
func printAddPersonHint(err error, email string) {
	if !errors.Is(err, invite.ErrPersonNotFound) {
		return
	}
	fmt.Fprintf(os.Stderr, "purser: to record them: purser person add --name NAME --email %s\n", email)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
