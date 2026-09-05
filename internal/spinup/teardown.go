package spinup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Einlanzerous/purser/internal/model"
)

// Teardown is the spin-up axis's inverse (PRSR-34). Everything above stands an
// edge up from a spec; this takes one down from Purser's records.
//
// It works from rows rather than from a spec, and that is the whole shape of it.
// A ServiceSpec says what a service's edge *should* be; a service_resource row
// says what Purser actually put there and what id it holds for it — which is the
// only thing a teardown may target, because deleting a record somebody made by
// hand is not a mistake re-running fixes. So there is no spec argument, no
// Inspect, and no upstream read on the plan path at all.
//
// Three things it inherits, each learned somewhere else first:
//
//   - **Preview by default**, more strongly than Ensure needs it. Ensure previews
//     because one of its three steps is a read-modify-write of shared state;
//     every step here is destructive. And like `offboard` — the person axis's
//     one genuinely destructive command — a dry run makes **no provisioner call
//     at all**, not even a read-only one. It has nothing to ask: the rows are
//     the plan.
//   - **A teardown that didn't happen must never be recorded as one.** Only a
//     successful Teardown marks the row removed; failed, unavailable and refused
//     leave it active so the next run retries (PRSR-17). The lie outlives the
//     error message, and the row is what the next run reads.
//   - **Per-resource failures must not abort the spec.** Every outcome is a
//     finding, for the reason ensureOne returns none either: once a step has
//     changed the edge, aborting discards the findings so far and leaves the
//     operator with one error line and no idea what went.

// ErrHostnameNotThisService is the answer to PRSR-34's open question: a
// service_resource row proves Purser created something at a hostname, and does
// not prove the hostname is still that service's to remove.
//
// Purser cannot settle that from the outside — a spec whose Key was reassigned,
// a hostname now fronting something else, a record edited by hand since, each
// wants a different answer and none of them is visible from a row. What it can
// do is require two coordinates that must agree: the operator names the service
// they believe owns the hostname, and a disagreement with Purser's own records
// refuses the whole run rather than removing anything. That is `offboard`'s
// shape — `--email` required, always one person, no bulk mode — and it is a
// stronger guard here than there, because the account row it compares against
// is unambiguous and these rows are not.
//
// The comparison is meaningful rather than ceremonial because service_key is
// kept current: UpsertServiceResource overwrites it on conflict, and ensureOne
// adopts on `rec.ServiceKey != t.Spec.Key` for exactly this reason. So a
// hostname that changed hands has rows naming its new owner from the moment
// somebody ran a spin-up for it, and this is what turns that into a refusal
// instead of a deletion.
//
// A sentinel because the HTTP surface has to tell it from an outage to choose a
// status code, and matching on the message would make that depend on the
// wording.
var ErrHostnameNotThisService = errors.New("spinup: hostname is recorded to another service")

// TeardownStatus is what happened to one recorded resource, or what would happen
// under Apply.
//
// Its own type rather than a reuse of StepStatus, which shares five of these
// values and means something different by most of the rest. `adopt`, `create`,
// `update`, `missing`, `orphaned` and `ok` have no reading on this path, and
// `remove`, `gone` and `none` have none on that one — and the two Results answer
// "is this fine?" from opposite lists. One enum spanning both would put every
// consumer in the position PRSR-21 took the person axis out of: reading a status
// and then remembering which direction it was travelling in.
type TeardownStatus string

const (
	// TeardownRemove — Purser holds an active record here and Apply removes it.
	// The one acting status, and the only one Pending counts.
	TeardownRemove TeardownStatus = "remove"
	// TeardownGone — Purser recorded one here and has already torn it down. The
	// resource is gone and the row says so, so there is nothing to do.
	//
	// Distinct from TeardownNone, which the same line would otherwise cover: a
	// re-run of a completed teardown should say "already removed, on this date",
	// not "Purser never had one", which reads like the resource was somebody
	// else's all along.
	TeardownGone TeardownStatus = "gone"
	// TeardownNone — Purser holds no record of this kind at this hostname, so
	// there is nothing here it may remove.
	//
	// Emphatically not "there is nothing here". A row exists only for a resource
	// Purser created (see model.ServiceResource), so this says Purser put
	// nothing here — and if something *is* there it belongs to somebody else,
	// which is the reason a teardown may not go looking for it by hostname.
	TeardownNone TeardownStatus = "none"
	// TeardownBlocked — this removal was held back because one it depends on has
	// not landed. See teardownDependsOn.
	TeardownBlocked TeardownStatus = "blocked"
	// TeardownUnavailable — no provisioner for the kind, or one that isn't
	// configured. Nothing broke, nothing was removed, and the row stays active.
	TeardownUnavailable TeardownStatus = "unavailable"
	// TeardownRefused — the provisioner read upstream successfully and came back
	// with something it will not act on. Nothing was removed and the row stays
	// active; re-running repeats this until it is fixed upstream.
	//
	// The tunnel is what it is for here as on the way up: an ingress document
	// whose catch-all is not last, or one belonging to a locally-managed tunnel,
	// is not evidence about what is served — so removing a route from it and
	// reporting the route gone would be a removal recorded over a live one.
	TeardownRefused TeardownStatus = "refused"
	// TeardownFailed — the removal was attempted and errored. The row stays
	// active, so the next run retries it.
	TeardownFailed TeardownStatus = "failed"
	// TeardownRemovedNotRecorded — upstream lost the resource and the row saying
	// so did not land.
	//
	// Its own status for the reason offboard's revoked-not-recorded and Ensure's
	// applied-not-recorded are (PRSR-17, PRSR-27): it points the opposite way
	// from failed. Failed means the resource is still there. This means it is
	// gone and Purser's records disagree — so `person show`'s equivalent here,
	// and the next spin-up's adopt, will both read a live-looking row for
	// something that no longer exists.
	TeardownRemovedNotRecorded TeardownStatus = "removed-not-recorded"
)

// TeardownFinding is one resource kind's verdict.
type TeardownFinding struct {
	Kind        model.ResourceKind
	DisplayName string
	Status      TeardownStatus
	// Detail describes what is recorded here, or what was just taken away — the
	// line an operator reads to check that what is about to go is what they
	// meant.
	Detail string
	// ExternalID is the recorded upstream id, when the kind has one. Empty for a
	// tunnel route by nature, not by omission: the configuration is one document
	// per tunnel, so a route's handle is (tunnel, hostname).
	ExternalID string
	// Warning is trouble around a removal that nonetheless happened — see
	// Removal.Warning. Distinct from Err, which belongs to a step that did not
	// do what it said.
	Warning string
	// Applied reports that this run removed something and recorded it. Always
	// false on a dry run.
	Applied bool
	Err     string
}

// TeardownRequest is one hostname's teardown.
type TeardownRequest struct {
	// ServiceKey is who the operator believes owns the hostname. Required, and
	// checked against the records before anything is removed — see
	// ErrHostnameNotThisService.
	ServiceKey string
	// Hostname is what to tear down. This axis's identity key, so it is the
	// whole target: there is no per-kind selection, because "take this hostname
	// down" is the request and taking half of it down is not a smaller version
	// of it.
	Hostname string
	// Apply turns the plan into deletions. False is a pure dry run, and unlike
	// Ensure's it makes no upstream call whatsoever.
	Apply bool
}

// Validate reports whether the request names something that can be looked up,
// and returns the normalized form the orchestrator should use.
//
// It exists for ServiceSpec.Validate's reason, one direction over: a surface
// calls it to decide whose fault a refusal is — a malformed request is the
// caller's and a failed record read is an outage — and Teardown calls it again
// because it is the orchestrator's own precondition and not something a surface
// may skip.
//
// Normalizing here rather than at each surface is what stops the CLI and the
// HTTP API disagreeing about what a padded or differently-cased hostname means.
// Both keys are folded exactly as ServiceSpec.Normalized folds them, which is
// not a nicety: they are compared against the rows a spin-up wrote, and a
// hostname folded one way going up and another coming down simply fails to find
// what it was going to remove.
func (r TeardownRequest) Validate() (TeardownRequest, error) {
	r.ServiceKey = normalizeKey(r.ServiceKey)
	r.Hostname = normalizeHostname(r.Hostname)
	if r.ServiceKey == "" {
		return r, fmt.Errorf("spinup: teardown needs the service key that owns the hostname — it is checked against Purser's records before anything is removed")
	}
	if r.Hostname == "" {
		return r, fmt.Errorf("spinup: teardown needs a hostname")
	}
	// Validated even though a malformed hostname cannot have rows. Without it,
	// "argosy.zerogravity.industries/admin" matches nothing and reports a clean
	// sweep of a hostname that was never spelled that way — a typo answered with
	// the most reassuring output this command can produce.
	if err := validHostname(r.Hostname); err != nil {
		return r, err
	}
	return r, nil
}

// TeardownResult is the whole plan, or the whole teardown.
type TeardownResult struct {
	ServiceKey string
	Hostname   string
	Findings   []TeardownFinding
	Applied    bool
}

// Counts summarizes findings by status, for a one-line summary.
func (r *TeardownResult) Counts() map[TeardownStatus]int {
	out := map[TeardownStatus]int{}
	for _, f := range r.Findings {
		out[f.Status]++
	}
	return out
}

// Changed reports how many resources this run actually removed and recorded.
// Zero on every dry run.
func (r *TeardownResult) Changed() int {
	n := 0
	for _, f := range r.Findings {
		if f.Applied {
			n++
		}
	}
	return n
}

// Pending reports how many removals still want doing — what makes "nothing to
// do" distinguishable from "re-run with --apply".
//
// Only `remove` counts. Blocked, unavailable, refused and failed are excluded
// for Result.Pending's reason: re-running with the flag fixes none of them,
// because the missing flag was never why they didn't happen. So a zero here is
// not the claim that the hostname's edge is gone — NeedsAttention is.
func (r *TeardownResult) Pending() int {
	n := 0
	for _, f := range r.Findings {
		if f.Status == TeardownRemove && !f.Applied {
			n++
		}
	}
	return n
}

// NeedsAttention reports the resources in a state a person has to resolve — the
// answer to "is this hostname's edge gone?", which neither Pending nor Changed
// answers.
//
// It lives on the result rather than in a renderer so the CLI's exit code and
// the HTTP response cannot drift about what counts as fine, which is exactly
// what Result.NeedsAttention exists for on the way up.
//
// `removed-not-recorded` is included even though the resource *is* gone: Purser
// now holds an active row for something that does not exist, so a later spin-up
// reads it as a record to adopt and a later teardown targets an id nobody can
// delete. The edge is right and the books are wrong, which is a thing somebody
// has to put back.
func (r *TeardownResult) NeedsAttention() []TeardownFinding {
	var out []TeardownFinding
	for _, f := range r.Findings {
		switch f.Status {
		case TeardownBlocked, TeardownUnavailable, TeardownRefused, TeardownFailed, TeardownRemovedNotRecorded:
			out = append(out, f)
		}
	}
	return out
}

// cleared reports whether this finding leaves its resource gone, so a removal
// ordered behind it may proceed. It is the predicate behind TeardownBlocked, and
// the mirror of StepFinding.inPlace.
//
// TeardownRemove counts, and the reasoning is inPlace's read backwards: it only
// *survives* as that status when the removal landed — an apply that failed
// reports TeardownFailed — and on a dry run it describes what the apply is going
// to do, which is what stops a plan of a whole hostname reporting two blocked
// steps behind one it has not been asked to perform yet.
//
// TeardownNone counts, and it is the one worth arguing. Purser holds no record,
// so it has nothing to remove and nothing it can do about the hostname still
// resolving through a record it did not create. Blocking there would hold the
// remaining steps for ever with no command to type — the prescribe-a-provable-
// no-op mistake offboard's SSO warning exists to avoid, which teaches an
// operator to ignore the signal that matters. It is said out loud on the
// dependent step instead (see danglingDNSWarning), because it *is* the shape
// that leaves a service reachable with its gate taken away.
//
// TeardownRemovedNotRecorded counts too: the resource is gone, and only the
// bookkeeping isn't.
func (f TeardownFinding) cleared() bool {
	switch f.Status {
	case TeardownRemove, TeardownGone, TeardownNone, TeardownRemovedNotRecorded:
		return true
	}
	return false
}

// teardownDependsOn reports which removals must have landed before kind may be
// removed. It is ServiceSpec.dependsOn inverted, and it is what makes
// model.TeardownOrder more than a preference.
//
// Everything depends on the DNS record going first, because the record is what
// makes the hostname live and the other two are inert once it is gone:
//
//   - Remove the Access application while the hostname still resolves and a
//     gated service is reachable *ungated*, for as long as the record takes to
//     go. That is the ungated-exposure window KindOrder exists to prevent,
//     approached from the other side, and it is not self-announcing: the service
//     works, it just lets everyone in.
//   - Remove the ingress route while the hostname still resolves and a tunnelled
//     service answers 502. Noisy and harmless by comparison — and blocked all the
//     same, because dependsOn blocks its mirror image on the way up and an
//     asymmetry here would be one nobody could derive.
//
// It takes no spec, unlike dependsOn, and cannot: a teardown has no spec. That
// costs one distinction. On the way up a *bookmark* Access app is deliberately
// not a DNS prerequisite, because its absence costs an icon rather than a gate —
// but a service_resource row records the kind, not whether the application was a
// gate or a launcher tile, so this side cannot tell them apart. Blocking both is
// the only answer available, and it is the right direction to be wrong in: being
// wrong about a bookmark costs a re-run, and being wrong about a gate costs an
// ungated service.
func teardownDependsOn(kind model.ResourceKind) []model.ResourceKind {
	if kind == model.ResourceDNSRecord {
		return nil
	}
	return []model.ResourceKind{model.ResourceDNSRecord}
}

// Teardown removes a hostname's recorded edge, or — by default — reports what it
// would remove.
//
// It returns an error only for a request that cannot be attempted at all: a
// missing or malformed identifier, a failed read of Purser's own records, or the
// hostname being recorded to another service. Everything a provisioner did or
// failed to do is a finding.
func (s *Service) Teardown(ctx context.Context, req TeardownRequest) (*TeardownResult, error) {
	req, err := req.Validate()
	if err != nil {
		return nil, err
	}
	key, hostname := req.ServiceKey, req.Hostname

	recorded, err := s.store.ServiceResourcesForHostname(ctx, hostname)
	if err != nil {
		return nil, err
	}
	active := make(map[model.ResourceKind]model.ServiceResource, len(recorded))
	removed := make(map[model.ResourceKind]model.ServiceResource, len(recorded))
	for _, r := range recorded {
		if r.Status == model.ResourceActive {
			active[r.Kind] = r
		} else {
			removed[r.Kind] = r
		}
	}
	if err := checkOwnership(key, hostname, active); err != nil {
		return nil, err
	}

	target := Target{Spec: ServiceSpec{Key: key, Hostname: hostname}}

	res := &TeardownResult{ServiceKey: key, Hostname: hostname, Applied: req.Apply}
	// Findings so far, so a removal can see whether the ones it depends on
	// landed. TeardownOrder is the order they are performed in, so a dependency
	// is always decided before its dependent is reached.
	done := make(map[model.ResourceKind]TeardownFinding, len(model.KindOrder))
	for _, kind := range model.TeardownOrder() {
		f := s.teardownOne(ctx, target, kind, active, removed, req.Apply, done)
		done[kind] = f
		res.Findings = append(res.Findings, f)
	}
	return res, nil
}

// checkOwnership refuses a hostname Purser attributes to a different service.
//
// Every active row is checked, not just the first: a hostname whose kinds
// disagree with *each other* about service_key has been half-reassigned, which
// is the same question with a worse answer, and one of them will disagree with
// whatever the operator typed.
//
// Removed rows are not checked. They are the history of a hostname that has
// changed hands, which is a thing that legitimately happened and not a reason to
// refuse taking down what is there now.
func checkOwnership(key, hostname string, active map[model.ResourceKind]model.ServiceResource) error {
	wrong := recordedToOthers(active, key)
	if len(wrong) == 0 {
		return nil
	}
	return fmt.Errorf("%w: a teardown of %s was asked for as %q, but %s — refusing to remove a hostname Purser attributes to another service; run a spin-up for whoever owns it, or fix the rows, so that two answers agree before anything is deleted",
		ErrHostnameNotThisService, hostname, key, strings.Join(wrong, ", "))
}

// recordedToOthers lists the active rows recorded to none of the allowed
// services, in KindOrder, each as `<kind> is recorded to "<key>"` — the
// comparison the teardown's refusal and the spin-up's (checkSpecOwnership,
// PRSR-48) share, stated once so the two surfaces cannot drift about what
// "recorded to another service" means. An empty allowed key matches nothing.
func recordedToOthers(active map[model.ResourceKind]model.ServiceResource, allowed ...string) []string {
	var wrong []string
	for _, kind := range model.KindOrder {
		rec, ok := active[kind]
		if !ok || permitted(rec.ServiceKey, allowed) {
			continue
		}
		wrong = append(wrong, fmt.Sprintf("%s is recorded to %q", kind, rec.ServiceKey))
	}
	return wrong
}

func permitted(key string, allowed []string) bool {
	for _, a := range allowed {
		if a != "" && a == key {
			return true
		}
	}
	return false
}

// teardownOne decides and, under apply, performs one removal.
//
// Like ensureOne it returns no error: every outcome is a finding.
func (s *Service) teardownOne(ctx context.Context, t Target, kind model.ResourceKind, active, removed map[model.ResourceKind]model.ServiceResource, apply bool, done map[model.ResourceKind]TeardownFinding) TeardownFinding {
	f := TeardownFinding{Kind: kind, DisplayName: string(kind)}
	prov, registered := s.registry.Get(kind)
	if registered {
		f.DisplayName = prov.DisplayName()
	}

	rec, has := active[kind]
	if !has {
		// Nothing active to remove. Which of the two "nothing" answers this is
		// matters to whoever typed the command, so it is not one status.
		if gone, ever := removed[kind]; ever {
			f.Status, f.ExternalID = TeardownGone, gone.ExternalID
			f.Detail = fmt.Sprintf("already removed%s", stamp(gone))
			return f
		}
		f.Status = TeardownNone
		f.Detail = "Purser recorded none here, so it has none to remove — anything upstream at this hostname is somebody else's"
		return f
	}

	f.ExternalID = rec.ExternalID
	f.Status, f.Detail = TeardownRemove, recordedDetail(rec)

	// Both refusals below are settled before the dry run returns, and neither
	// contacts anything — so the plan and the apply agree about a step that was
	// never going to happen. This is the half of "preview by default" that has
	// to be built rather than assumed: the plan path makes no upstream call, so
	// unlike Ensure it cannot learn from an Inspect that a provisioner is
	// unconfigured.
	if !registered {
		// A resource this build cannot take away. Never "nothing to do": it is
		// still there, and the row still says so.
		f.Status = TeardownUnavailable
		f.Err = fmt.Sprintf("no provisioner registered for %q in this build", kind)
		return f
	}
	if err := CanTeardown(prov, t); err != nil {
		f.Status, f.Err = teardownRefusalStatus(err), err.Error()
		return f
	}

	// Held back rather than performed. Unlike Ensure there is nothing to exempt:
	// every status that reaches here is the acting one.
	if unmet := unmetTeardownDeps(kind, done); len(unmet) > 0 {
		f.Status, f.Detail = TeardownBlocked, blockedTeardownDetail(unmet)
		return f
	}
	f.Warning = danglingDNSWarning(kind, done)

	if !apply {
		return f // dry run: no provisioner is called at all
	}

	rm, err := prov.Teardown(ctx, t, rec)
	if err != nil {
		f.Status, f.Err = teardownFailureStatus(err), err.Error()
		return f
	}
	// Overwritten, not merged: Detail described what was recorded, and after the
	// call the useful line is what happened to it. A provisioner that returns
	// none leaves the row's description standing, which is still true.
	if rm.Detail != "" {
		f.Detail = rm.Detail
	}
	if rm.Warning != "" {
		// Appended rather than assigned: the dangling-DNS note above is this
		// run's own observation about a hostname that may still resolve, and the
		// provisioner's is about some other service on the same tunnel. Losing
		// either to the other is losing the half that was not being looked for.
		f.Warning = joinWarnings(f.Warning, rm.Warning)
	}

	if err := s.store.MarkServiceResourceRemoved(ctx, rec.ID); err != nil {
		// The resource is gone and the bookkeeping isn't — the opposite advice
		// from failed, and never swallowed into it.
		f.Status, f.Err = TeardownRemovedNotRecorded, err.Error()
		return f
	}
	f.Applied = true
	return f
}

// teardownRefusalStatus buckets a refusal that arrived before anything was
// attempted. It is separate from teardownFailureStatus because there is no
// `failed` in it: nothing was tried, so an answer this side of the attempt can
// only be one of the two "and nothing was done" statuses.
func teardownRefusalStatus(err error) TeardownStatus {
	if IsRefused(err) {
		return TeardownRefused
	}
	return TeardownUnavailable
}

// teardownFailureStatus buckets what a provisioner returned. The three want
// different things from an operator — configure Purser, fix something upstream,
// or just run it again — and all three leave the row active, so the next run
// retries.
func teardownFailureStatus(err error) TeardownStatus {
	switch {
	case IsUnavailable(err):
		return TeardownUnavailable
	case IsRefused(err):
		return TeardownRefused
	default:
		return TeardownFailed
	}
}

// unmetTeardownDeps returns the removals kind is ordered behind that this run
// has not cleared.
func unmetTeardownDeps(kind model.ResourceKind, done map[model.ResourceKind]TeardownFinding) []model.ResourceKind {
	var unmet []model.ResourceKind
	for _, dep := range teardownDependsOn(kind) {
		if f, decided := done[dep]; !decided || !f.cleared() {
			unmet = append(unmet, dep)
		}
	}
	return unmet
}

// blockedTeardownDetail explains which removals held this one back, naming them
// so the operator doesn't have to infer it from the other lines.
func blockedTeardownDetail(unmet []model.ResourceKind) string {
	names := make([]string, len(unmet))
	for i, k := range unmet {
		names[i] = string(k)
	}
	return fmt.Sprintf("held back: %s did not go, and the hostname still resolves — removing this in front of it is what the ordering exists to prevent",
		strings.Join(names, " and "))
}

// danglingDNSWarning is the note for the one case teardownDependsOn deliberately
// lets through: Purser holds no DNS record for the hostname, so the removal
// proceeds, and nothing here stopped the name resolving.
//
// It is the shape offboard's "revoking Switchyard does not close its door"
// warning has, and it is here for the same reason — a command that looks
// finished while leaving a working door open should say so itself. Only the
// Access application gets it: a tunnel route removed from under a live hostname
// produces a 502, which announces itself, and the thing this covers is the gate,
// which does not.
func danglingDNSWarning(kind model.ResourceKind, done map[model.ResourceKind]TeardownFinding) string {
	if kind != model.ResourceAccessApp {
		return ""
	}
	if f, decided := done[model.ResourceDNSRecord]; !decided || f.Status != TeardownNone {
		return ""
	}
	return "Purser holds no DNS record for this hostname, so nothing here stopped it resolving. If a record created outside Purser still publishes it and this application was the gate, the service is reachable ungated once this is removed."
}

// joinWarnings puts two warnings on one field without either swallowing the
// other.
func joinWarnings(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + " " + b
}

// recordedDetail describes the row a removal is about to target, which on the
// plan path is the whole of what an operator has to check the command against —
// there is no upstream read to describe.
func recordedDetail(rec model.ServiceResource) string {
	var b strings.Builder
	if rec.ExternalID != "" {
		fmt.Fprintf(&b, "recorded %s", rec.ExternalID)
	} else {
		// A tunnel route, which has no id: its handle is (tunnel, hostname), so
		// naming the parent is naming the resource rather than decorating it.
		fmt.Fprintf(&b, "recorded for %s", rec.Hostname)
	}
	if rec.ParentID != "" {
		fmt.Fprintf(&b, " in %s", rec.ParentID)
	}
	fmt.Fprintf(&b, ", created %s", rec.CreatedAt.UTC().Format("2006-01-02"))
	return b.String()
}

// stamp renders when a row was torn down, or nothing if the column is unset —
// which a pre-migration-0007 row cannot be, but a hand-edited one can.
func stamp(rec model.ServiceResource) string {
	if rec.RemovedAt == nil {
		return ""
	}
	return ", on " + rec.RemovedAt.UTC().Format("2006-01-02")
}
