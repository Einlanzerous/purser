package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/Einlanzerous/purser/internal/invite"
	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/store"
)

// dateFormat is what timestamps look like in the tables. The full instant is in
// --json, where a machine reads it; a human comparing "who was set up when"
// wants the column narrow enough to scan.
const dateFormat = "2006-01-02"

// stampFormat is for the invite history, where the date alone is not enough:
// re-running an invite is the routine case — that is what "retries only what
// failed" means in practice — and two runs a minute apart while someone fixes a
// token would otherwise render as identical rows in a list whose whole job is
// ordering them. Seconds, because minutes lose exactly that case.
const stampFormat = "2006-01-02 15:04:05"

// dash stands in for a field with no value, so an empty column reads as
// "nothing here" rather than as a rendering bug.
const dash = "—"

// runPersonList is `purser person list`: the roster, read from local records
// (PRSR-24).
//
//	purser person list                      # everyone, with their active services
//	purser person list --to switchyard      # who holds Switchyard?
//	purser person list --all --json         # everything, for an agent
func runPersonList(args []string) {
	fs := flag.NewFlagSet("person list", flag.ExitOnError)
	var (
		to     = fs.String("to", "", "limit to people holding these services, comma-separated")
		typ    = fs.String("type", "", "limit to one identity kind: human | agent")
		all    = fs.Bool("all", false, "include deprovisioned and stale accounts, not just active ones")
		asJSON = fs.Bool("json", false, "emit JSON on stdout instead of a table")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, personListUsage)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Reads person, account and service. Calls no connectors, so it answers")
		fmt.Fprintln(os.Stderr, "while upstream is unreachable — `purser audit` is the command that")
		fmt.Fprintln(os.Stderr, "compares these records against upstream reality.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	requireNoOperands(fs, personListUsage)

	// Checked before setup(), so a bad flag costs an exit 2 rather than a
	// database connect and a migration run.
	if _, err := invite.ParsePersonType(*typ); err != nil {
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

	res, err := a.svc.Roster(ctx, invite.RosterRequest{
		Services:        splitCSV(*to),
		Type:            model.PersonType(*typ),
		IncludeInactive: *all,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		// A mistyped service is a flag to retype, not an outage — the same exit
		// as a mistyped --type, so a script can't read one as infrastructure
		// failure and the other as user error.
		if errors.Is(err, invite.ErrUnknownService) {
			os.Exit(2)
		}
		os.Exit(1)
	}

	if *asJSON {
		writeJSON(newRosterDTO(res))
	} else {
		printRoster(res)
	}
	printRosterSummary(res)
}

// runPersonShow is `purser person show --email …`: one person in full.
func runPersonShow(args []string) {
	fs := flag.NewFlagSet("person show", flag.ExitOnError)
	var (
		email  = fs.String("email", "", "the person's email (required — the identity key)")
		asJSON = fs.Bool("json", false, "emit JSON on stdout instead of a table")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, personShowUsage)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Their person row, every account with its status, and their invite")
		fmt.Fprintln(os.Stderr, "history. Local records only — no connector is called, and no secret is")
		fmt.Fprintln(os.Stderr, "printed: credentials are shown once, at invite time.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	requireNoOperands(fs, personShowUsage)

	// Kept, not discarded: the normalized address is what the lookup will key
	// on, so the hint below quotes the same string rather than a second, weaker
	// normalization of the raw flag.
	wanted, err := invite.NormalizeEmail(*email)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		fmt.Fprintln(os.Stderr, personShowUsage)
		os.Exit(2)
	}

	ctx := context.Background()
	a, err := setup(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		os.Exit(1)
	}
	defer a.cleanup()

	d, err := a.svc.PersonDetail(ctx, wanted)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purser: %v\n", err)
		printAddPersonHint(err, wanted)
		os.Exit(1)
	}

	if *asJSON {
		writeJSON(newPersonDetailDTO(d))
		return
	}
	printPersonDetail(d)
}

// --- table rendering ---

// printRoster writes the roster to stdout and nothing else, so `purser person
// list > roster.txt` captures rows a script can read. An empty roster is a
// header and no rows; the count that explains it belongs to the summary, on
// stderr, rather than to a parenthetical row pretending to be data.
func printRoster(res *invite.RosterResult) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tEMAIL\tTYPE\tSERVICES\tSINCE")
	for _, e := range res.Entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			e.Person.Name, orDash(e.Person.Email), e.Person.Type,
			servicesCell(e.Accounts), e.Person.CreatedAt.Format(dateFormat))
	}
	_ = w.Flush()
}

// printRosterSummary reports the count and — the part that matters — says so
// when the default filter dropped something. Hidden is zero whenever --all was
// passed, so the count alone is the whole condition.
func printRosterSummary(res *invite.RosterResult) {
	n := len(res.Entries)
	noun := "people"
	if n == 1 {
		noun = "person"
	}
	fmt.Fprintf(os.Stderr, "\n%d %s\n", n, noun)
	if res.Hidden > 0 {
		fmt.Fprintf(os.Stderr, "%d non-active account%s hidden (deprovisioned or stale) — pass --all to include %s\n",
			res.Hidden, plural(res.Hidden), them(res.Hidden))
	}
}

// servicesCell renders a person's services as one column. Non-active accounts
// carry their status inline: they only appear under --all, and an unlabelled
// key there would read as access the person still holds.
func servicesCell(accounts []store.AccountRecord) string {
	if len(accounts) == 0 {
		return dash
	}
	parts := make([]string, 0, len(accounts))
	for _, a := range accounts {
		if a.Status == model.AccountActive {
			parts = append(parts, a.ServiceKey)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s(%s)", a.ServiceKey, a.Status))
	}
	return strings.Join(parts, ", ")
}

// printPersonDetail writes one person's full local record to stdout.
func printPersonDetail(d *invite.PersonDetail) {
	p := d.Person
	fmt.Printf("%s <%s>\n", p.Name, orDash(p.Email))
	fmt.Printf("%s · id %s · recorded %s · updated %s\n",
		p.Type, p.ID, p.CreatedAt.Format(dateFormat), p.UpdatedAt.Format(dateFormat))

	fmt.Println("\nACCOUNTS")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tSTATUS\tUSERNAME\tEXTERNAL ID\tRECORDED\tUPDATED\tREVOKED")
	for _, a := range d.Accounts {
		revoked := dash
		if a.DeprovisionedAt != nil {
			revoked = a.DeprovisionedAt.Format(dateFormat)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			a.ServiceKey, a.Status, orDash(a.Username), orDash(a.ExternalID),
			a.CreatedAt.Format(dateFormat), a.UpdatedAt.Format(dateFormat), revoked)
	}
	if len(d.Accounts) == 0 {
		fmt.Fprintln(w, "(none recorded)")
	}
	_ = w.Flush()

	fmt.Println("\nINVITES")
	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WHEN\tDELIVERY\tROLE\tDELIVERED")
	for _, inv := range d.Invites {
		delivered := dash
		if inv.DeliveredAt != nil {
			delivered = inv.DeliveredAt.Format(stampFormat)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			inv.CreatedAt.Format(stampFormat), inv.Delivery, orDash(inv.Role), delivered)
	}
	if len(d.Invites) == 0 {
		fmt.Fprintln(w, "(none)")
	}
	_ = w.Flush()

	if len(d.Accounts) == 0 {
		fmt.Fprintln(os.Stderr, "\nNothing recorded against them yet:")
		hw := tabwriter.NewWriter(os.Stderr, 0, 0, 2, ' ', 0)
		fmt.Fprintf(hw, "  purser audit --email %s\t# do they already hold something upstream?\n", p.Email)
		fmt.Fprintf(hw, "  purser invite --name %q --email %s\t# provision them\n", p.Name, p.Email)
		_ = hw.Flush()
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return dash
	}
	return s
}

func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// --- JSON ---

// The --json shapes. `person list` and `person show` share personDTO and
// accountDTO deliberately: an agent reaching for the roster before an invite —
// the case that drove this — can read either command's output with one code
// path.
//
// None of these carry credential material, and none of them can: they are built
// from store.AccountRecord, which has no secret_hash and no secret_ref field to
// copy across. That is the guarantee, rather than a rule about what to omit
// here (PRSR-24).
type rosterDTO struct {
	People []rosterEntryDTO `json:"people"`
	// HiddenAccounts is the same count the table prints to stderr, and it is
	// here for the same reason: an empty `people` is the one result a consumer
	// cannot interpret without it. `--to lyceum` returning nothing has to be
	// readable as "nobody has Lyceum any more" rather than "nobody ever did",
	// and stderr is exactly what a `| jq` pipeline throws away.
	//
	// Never omitempty: "the field is absent" and "the value is zero" would then
	// be the same wire form, which is the ambiguity the field exists to remove.
	HiddenAccounts int `json:"hidden_accounts"`
}

type rosterEntryDTO struct {
	Person   personDTO    `json:"person"`
	Accounts []accountDTO `json:"accounts"`
}

type personDetailDTO struct {
	Person   personDTO    `json:"person"`
	Accounts []accountDTO `json:"accounts"`
	Invites  []inviteDTO  `json:"invites"`
}

type personDTO struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type accountDTO struct {
	Service     string    `json:"service"`
	DisplayName string    `json:"display_name"`
	Status      string    `json:"status"`
	Username    string    `json:"username,omitempty"`
	ExternalID  string    `json:"external_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// DeprovisionedAt is when access was last revoked; absent if never. It
	// survives a re-provision on purpose (migration 0006), so an active account
	// may still carry one — read status for the current state.
	DeprovisionedAt *time.Time `json:"deprovisioned_at,omitempty"`
}

type inviteDTO struct {
	ID          uuid.UUID  `json:"id"`
	Delivery    string     `json:"delivery"`
	Role        string     `json:"role,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

func newRosterDTO(res *invite.RosterResult) rosterDTO {
	// Built empty rather than nil so an empty roster encodes as [] — a consumer
	// iterating the field shouldn't have to special-case null.
	out := rosterDTO{
		People:         make([]rosterEntryDTO, 0, len(res.Entries)),
		HiddenAccounts: res.Hidden,
	}
	for _, e := range res.Entries {
		out.People = append(out.People, rosterEntryDTO{
			Person:   newPersonDTO(e.Person),
			Accounts: newAccountDTOs(e.Accounts),
		})
	}
	return out
}

func newPersonDetailDTO(d *invite.PersonDetail) personDetailDTO {
	out := personDetailDTO{
		Person:   newPersonDTO(d.Person),
		Accounts: newAccountDTOs(d.Accounts),
		Invites:  make([]inviteDTO, 0, len(d.Invites)),
	}
	for _, inv := range d.Invites {
		out.Invites = append(out.Invites, inviteDTO{
			ID:          inv.ID,
			Delivery:    string(inv.Delivery),
			Role:        inv.Role,
			CreatedAt:   inv.CreatedAt,
			DeliveredAt: inv.DeliveredAt,
		})
	}
	return out
}

func newPersonDTO(p model.Person) personDTO {
	return personDTO{
		ID:        p.ID,
		Name:      p.Name,
		Email:     p.Email,
		Type:      string(p.Type),
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
}

func newAccountDTOs(accounts []store.AccountRecord) []accountDTO {
	out := make([]accountDTO, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, accountDTO{
			Service:     a.ServiceKey,
			DisplayName: a.DisplayName,
			Status:      string(a.Status),
			Username:    a.Username,
			ExternalID:  a.ExternalID,
			CreatedAt:   a.CreatedAt,
			UpdatedAt:   a.UpdatedAt,

			DeprovisionedAt: a.DeprovisionedAt,
		})
	}
	return out
}

// writeJSON puts the document on stdout and nothing else, so `| jq` works. The
// summary and any warnings stay on stderr, where they don't corrupt the pipe.
func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "purser: encode json: %v\n", err)
		os.Exit(1)
	}
}
