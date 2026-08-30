package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/config"
	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

func step(kind model.ResourceKind, status spinup.StepStatus) spinup.StepFinding {
	return spinup.StepFinding{Kind: kind, DisplayName: string(kind), Status: status}
}

// The exit code is the only part of this command a script can read, so what it
// counts as "the edge is not as the spec asks" is worth pinning directly.
func TestProvisionExit(t *testing.T) {
	cases := []struct {
		name     string
		applied  bool
		findings []spinup.StepFinding
		want     int
	}{
		{
			name:    "an applied spin-up that landed",
			applied: true,
			findings: []spinup.StepFinding{
				step(model.ResourceTunnelRoute, spinup.StepSkipped),
				step(model.ResourceAccessApp, spinup.StepAdopt),
				step(model.ResourceDNSRecord, spinup.StepOK),
			},
			want: 0,
		},
		{
			// A plan with work outstanding exits 0: previewing succeeded at
			// previewing, and the pending count is what says there is more to
			// do. Otherwise a plan could never be run in a pipeline without
			// every un-applied step reading as a fault. This is offboard's rule.
			name: "a plan with work to do",
			findings: []spinup.StepFinding{
				step(model.ResourceTunnelRoute, spinup.StepCreate),
				step(model.ResourceAccessApp, spinup.StepCreate),
				step(model.ResourceDNSRecord, spinup.StepCreate),
			},
			want: 0,
		},
		{
			// Follows offboard rather than invite, and for the same reason. On
			// an invite, unavailable means nothing was granted and nobody is
			// harmed by waiting. Here it means a step of the edge does not
			// exist, so the hostname does not work.
			name:     "unavailable is not benign on this axis",
			findings: []spinup.StepFinding{step(model.ResourceDNSRecord, spinup.StepUnavailable)},
			want:     1,
		},
		{
			name:     "refused",
			findings: []spinup.StepFinding{step(model.ResourceTunnelRoute, spinup.StepRefused)},
			want:     1,
		},
		{
			name:     "unknown",
			findings: []spinup.StepFinding{step(model.ResourceDNSRecord, spinup.StepUnknown)},
			want:     1,
		},
		{
			name:     "blocked",
			findings: []spinup.StepFinding{step(model.ResourceDNSRecord, spinup.StepBlocked)},
			want:     1,
		},
		{
			name:     "failed",
			findings: []spinup.StepFinding{step(model.ResourceAccessApp, spinup.StepFailed)},
			want:     1,
		},
		{
			// The edge changed and Purser cannot tear down what it holds no id
			// for. Points the opposite way from failed, and is emphatically not
			// a success.
			name:     "applied but not recorded",
			applied:  true,
			findings: []spinup.StepFinding{step(model.ResourceDNSRecord, spinup.StepAppliedNotRecorded)},
			want:     1,
		},
		{
			// Orphaned is a report about a resource this spec does not manage.
			// It is worth saying and it is not this run failing.
			name:    "orphaned alone is not a failure",
			applied: true,
			findings: []spinup.StepFinding{
				step(model.ResourceTunnelRoute, spinup.StepOrphaned),
				step(model.ResourceDNSRecord, spinup.StepOK),
			},
			want: 0,
		},
		{
			// One bad step is enough, even surrounded by good ones.
			name:    "one failure among successes",
			applied: true,
			findings: []spinup.StepFinding{
				step(model.ResourceTunnelRoute, spinup.StepOK),
				step(model.ResourceAccessApp, spinup.StepFailed),
				step(model.ResourceDNSRecord, spinup.StepBlocked),
			},
			want: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &spinup.Result{Applied: tc.applied, Findings: tc.findings}
			if got := provisionExit(res); got != tc.want {
				t.Errorf("provisionExit = %d, want %d", got, tc.want)
			}
		})
	}
}

// A step whose whole story is in its Err gets "see below" in the table rather
// than an empty cell, because the errors are printed underneath in full and an
// empty DETAIL reads as "nothing to say about this one".
func TestProvisionDetail(t *testing.T) {
	cases := []struct {
		name string
		f    spinup.StepFinding
		want string
	}{
		{"an error with no detail points at the text below",
			spinup.StepFinding{Status: spinup.StepRefused, Err: "the catch-all is not last"}, "see below"},
		{"a detail wins, since it describes what is actually there",
			spinup.StepFinding{Status: spinup.StepUnknown, Detail: "3 records answer for this name", Err: "ambiguous"},
			"3 records answer for this name"},
		{"an ordinary step is its detail",
			spinup.StepFinding{Status: spinup.StepOK, Detail: "A → 100.64.0.7"}, "A → 100.64.0.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := provisionDetail(tc.f); got != tc.want {
				t.Errorf("provisionDetail = %q, want %q", got, tc.want)
			}
		})
	}
}

// A kind with no provisioner is reported `unavailable`, which reads like a
// deployment that needs configuring rather than a build that is missing a step
// — so a registration dropped from the composition root would be invisible in
// exactly the way this axis tries not to be. NewRegistry catches a *wrong* kind
// by panicking; nothing catches an absent one but this.
func TestSpinupRegistryCoversEveryKindTheOrchestratorWalks(t *testing.T) {
	reg := spinupRegistry(config.Config{})
	for _, kind := range model.KindOrder {
		if _, ok := reg.Get(kind); !ok {
			t.Errorf("no provisioner registered for %q — every step the orchestrator walks needs one", kind)
		}
	}
	if got, want := len(reg.Kinds()), len(model.KindOrder); got != want {
		t.Errorf("registered %d kinds, want %d", got, want)
	}
}

// Registered without credentials, which is deliberate: each provisioner reports
// the variable it is missing, which a generic stand-in could not.
func TestSpinupRegistryIsUnavailableRatherThanAbsentWhenUnconfigured(t *testing.T) {
	reg := spinupRegistry(config.Config{})
	p, ok := reg.Get(model.ResourceDNSRecord)
	if !ok {
		t.Fatal("the DNS provisioner should be registered even with no credentials")
	}
	_, err := p.Inspect(context.Background(), spinup.Target{})
	if !spinup.IsUnavailable(err) {
		t.Fatalf("an unconfigured provisioner reports unavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "PURSER_CF_ZONE_ID") {
		t.Errorf("the refusal should name what to set, got %v", err)
	}
}

// The whole reason a spec names a ref rather than carrying an opaque id: `dev`
// is a name a spec may legally use, and until PRSR-33 supplies its id it must
// resolve to a refusal. Falling back to prod would have a dev spin-up rewrite
// the production tunnel's shared ingress document.
func TestTunnelSetRefusesDevRatherThanFallingBackToProd(t *testing.T) {
	cfg := config.Config{}
	cfg.Cloudflare.TunnelID = "aef21667-0000-4000-8000-000000000001"
	ts := tunnelSet(cfg)

	id, err := ts.Resolve(spinup.TunnelProd)
	if err != nil || id != cfg.Cloudflare.TunnelID {
		t.Fatalf("prod should resolve to the configured id, got %q %v", id, err)
	}

	got, err := ts.Resolve(spinup.TunnelDev)
	if err == nil {
		t.Fatalf("dev is not wired yet and must refuse, got %q", got)
	}
	if got == cfg.Cloudflare.TunnelID {
		t.Fatal("dev must never fall back to the prod tunnel — a dev spin-up would rewrite prod's ingress")
	}
	if !errors.Is(err, spinup.ErrTunnelUnconfigured) {
		t.Errorf("the refusal must be recognisable without matching on its text, got %v", err)
	}
}

// The closing line is what an operator reads if they read nothing else, so the
// case that matters is the one where nothing is *pending* and the edge is still
// not up: Pending() excludes unavailable, refused, unknown and blocked, because
// --apply fixes none of them. Reading "0 pending" as "all good" reported "the
// edge already matches this spec" over a plan whose DNS step was unavailable —
// a hostname that does not resolve, described as a service that is up.
func TestProvisionOutcome(t *testing.T) {
	cases := []struct {
		name     string
		applied  bool
		findings []spinup.StepFinding
		want     string
	}{
		{
			name: "the trap: nothing pending, and nothing works either",
			findings: []spinup.StepFinding{
				step(model.ResourceTunnelRoute, spinup.StepSkipped),
				step(model.ResourceAccessApp, spinup.StepUnavailable),
				step(model.ResourceDNSRecord, spinup.StepUnavailable),
			},
			want: "Plan — there is nothing --apply would fix. The steps above need attention first.",
		},
		{
			name: "genuinely already up",
			findings: []spinup.StepFinding{
				step(model.ResourceTunnelRoute, spinup.StepSkipped),
				step(model.ResourceAccessApp, spinup.StepOK),
				step(model.ResourceDNSRecord, spinup.StepOK),
			},
			want: "Plan — nothing to do; the edge already matches this spec.",
		},
		{
			name: "work --apply would do",
			findings: []spinup.StepFinding{
				step(model.ResourceAccessApp, spinup.StepCreate),
				step(model.ResourceDNSRecord, spinup.StepCreate),
			},
			want: "Plan — nothing created or changed. Re-run with --apply to act on 2 steps.",
		},
		{
			name:     "one step, singular",
			findings: []spinup.StepFinding{step(model.ResourceDNSRecord, spinup.StepCreate)},
			want:     "Plan — nothing created or changed. Re-run with --apply to act on 1 step.",
		},
		{
			// A refused step is not pending either, and it is the case where
			// "re-run" is the actively wrong advice.
			name:     "refused is not pending, and not fine",
			findings: []spinup.StepFinding{step(model.ResourceTunnelRoute, spinup.StepRefused)},
			want:     "Plan — there is nothing --apply would fix. The steps above need attention first.",
		},
		{
			name:    "an apply reports what it changed",
			applied: true,
			findings: []spinup.StepFinding{
				{Kind: model.ResourceAccessApp, Status: spinup.StepAdopt, Applied: true},
				{Kind: model.ResourceDNSRecord, Status: spinup.StepCreate, Applied: true},
				step(model.ResourceTunnelRoute, spinup.StepSkipped),
			},
			want: "Applied 2 of 3.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := &spinup.Result{Applied: tc.applied, Findings: tc.findings}
			if got := provisionOutcome(res); got != tc.want {
				t.Errorf("provisionOutcome =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// --- prune (PRSR-46) --------------------------------------------------------

// An orphan still exits zero, with or without --prune in play. It does not
// falsify the claim the exit code makes, which is that the spec is satisfied —
// "and nothing else is here" is the rounding-up Result.NeedsAttention forbids.
// A run that deliberately keeps a resource the spec no longer names would
// otherwise exit non-zero for ever.
func TestProvisionExit_AnOrphanIsStillClean(t *testing.T) {
	res := &spinup.Result{Findings: []spinup.StepFinding{
		step(model.ResourceTunnelRoute, spinup.StepOrphaned),
		step(model.ResourceAccessApp, spinup.StepOK),
		step(model.ResourceDNSRecord, spinup.StepOK),
	}}
	if got := provisionExit(res); got != 0 {
		t.Errorf("exit %d, want 0", got)
	}
	// A *planned* prune is likewise clean — previewing succeeded at previewing —
	// but it is work outstanding, so it counts toward Pending where an orphan
	// does not.
	planned := &spinup.Result{Pruned: true, Findings: []spinup.StepFinding{
		step(model.ResourceTunnelRoute, spinup.StepPrune),
		step(model.ResourceDNSRecord, spinup.StepOK),
	}}
	if got := provisionExit(planned); got != 0 {
		t.Errorf("planned prune: exit %d, want 0", got)
	}
	if planned.Pending() != 1 {
		t.Errorf("Pending()=%d, want 1", planned.Pending())
	}
	if res.Pending() != 0 {
		t.Errorf("an orphan nobody asked about counts as pending: %d", res.Pending())
	}
}

// The resource is gone and Purser holds a live-looking row for it, which a later
// run reads as something to adopt. Somebody has to put that back.
func TestProvisionExit_PrunedNotRecordedIsNotClean(t *testing.T) {
	res := &spinup.Result{Applied: true, Pruned: true, Findings: []spinup.StepFinding{
		step(model.ResourceAccessApp, spinup.StepPrunedNotRecorded),
		step(model.ResourceDNSRecord, spinup.StepOK),
	}}
	if got := provisionExit(res); got != 1 {
		t.Errorf("exit %d, want 1", got)
	}
}

// A plan whose pending work includes deletions says so. "Act on 3 steps" over a
// line that is going to remove a live Access application is not a sentence
// anybody should read in a hurry.
func TestProvisionOutcome_APlanWithPrunesSaysSo(t *testing.T) {
	res := &spinup.Result{Pruned: true, Findings: []spinup.StepFinding{
		step(model.ResourceAccessApp, spinup.StepPrune),
		step(model.ResourceDNSRecord, spinup.StepOK),
	}}
	got := provisionOutcome(res)
	if !strings.Contains(got, "removal") {
		t.Errorf("outcome %q must say the pending work includes removals", got)
	}

	// And without --prune the wording is unchanged, since nothing is going.
	plain := &spinup.Result{Findings: []spinup.StepFinding{
		step(model.ResourceAccessApp, spinup.StepCreate),
	}}
	if strings.Contains(provisionOutcome(plain), "removal") {
		t.Errorf("outcome %q mentions removals on a run that has none", provisionOutcome(plain))
	}
}
