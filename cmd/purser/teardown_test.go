package main

import (
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/model"
	"github.com/Einlanzerous/purser/internal/spinup"
)

func removal(kind model.ResourceKind, status spinup.TeardownStatus) spinup.TeardownFinding {
	return spinup.TeardownFinding{Kind: kind, DisplayName: string(kind), Status: status}
}

func teardownResult(applied bool, fs ...spinup.TeardownFinding) *spinup.TeardownResult {
	return &spinup.TeardownResult{
		ServiceKey: "interlock", Hostname: "interlock.zerogravity.industries",
		Findings: fs, Applied: applied,
	}
}

// The exit code is the only part of this command a script can read, so what it
// counts as "the hostname's recorded edge is still there" is pinned directly.
func TestTeardownExit(t *testing.T) {
	cases := []struct {
		name     string
		applied  bool
		findings []spinup.TeardownFinding
		want     int
	}{
		{
			name:    "an applied teardown that landed",
			applied: true,
			findings: []spinup.TeardownFinding{
				{Kind: model.ResourceDNSRecord, Status: spinup.TeardownRemove, Applied: true},
				{Kind: model.ResourceAccessApp, Status: spinup.TeardownRemove, Applied: true},
				removal(model.ResourceTunnelRoute, spinup.TeardownNone),
			},
			want: 0,
		},
		{
			// A plan with removals outstanding exits 0: previewing succeeded at
			// previewing, and the pending count is what says there is more to
			// do. Otherwise a plan could never run in a pipeline without every
			// un-applied removal reading as a fault. offboard's rule.
			name: "a plan with work to do",
			findings: []spinup.TeardownFinding{
				removal(model.ResourceDNSRecord, spinup.TeardownRemove),
				removal(model.ResourceAccessApp, spinup.TeardownRemove),
			},
			want: 0,
		},
		{
			name:     "nothing was ever recorded here",
			findings: []spinup.TeardownFinding{removal(model.ResourceDNSRecord, spinup.TeardownNone)},
			want:     0,
		},
		{
			name:     "already torn down",
			findings: []spinup.TeardownFinding{removal(model.ResourceDNSRecord, spinup.TeardownGone)},
			want:     0,
		},
		{
			// Follows offboard rather than invite. On an invite, unavailable
			// means nothing was granted and waiting harms nobody. Here it means
			// a resource is still live while the operator has been told the
			// service is being taken down.
			name:     "unavailable is not benign on a removal",
			findings: []spinup.TeardownFinding{removal(model.ResourceDNSRecord, spinup.TeardownUnavailable)},
			want:     1,
		},
		{
			name:     "refused",
			findings: []spinup.TeardownFinding{removal(model.ResourceTunnelRoute, spinup.TeardownRefused)},
			want:     1,
		},
		{
			name:     "blocked behind a hostname that still resolves",
			findings: []spinup.TeardownFinding{removal(model.ResourceAccessApp, spinup.TeardownBlocked)},
			want:     1,
		},
		{
			name:     "failed",
			findings: []spinup.TeardownFinding{removal(model.ResourceDNSRecord, spinup.TeardownFailed)},
			want:     1,
		},
		{
			// The resource is gone and Purser's rows say otherwise, which a
			// later spin-up reads as something to adopt. Somebody has to put it
			// back, so it is not a clean exit.
			name:     "removed but not recorded",
			findings: []spinup.TeardownFinding{removal(model.ResourceDNSRecord, spinup.TeardownRemovedNotRecorded)},
			want:     1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := teardownExit(teardownResult(tc.applied, tc.findings...)); got != tc.want {
				t.Errorf("exit %d, want %d", got, tc.want)
			}
		})
	}
}

// The closing line is what an operator reads if they read nothing else, so it
// must not say the reassuring thing when the reassuring thing is not true.
// Pending() excludes every status --apply cannot fix, so "nothing pending" is
// not "the hostname is clear" — the mistake PRSR-31 shipped and fixed on the
// spin-up side of this same pair.
func TestTeardownOutcome_DoesNotSignOffOnAnUnconfiguredRun(t *testing.T) {
	res := teardownResult(false,
		removal(model.ResourceDNSRecord, spinup.TeardownUnavailable),
		removal(model.ResourceAccessApp, spinup.TeardownBlocked),
	)
	if res.Pending() != 0 {
		t.Fatalf("Pending()=%d — this test is about the case where it is zero", res.Pending())
	}
	got := teardownOutcome(res)
	if strings.Contains(got, "nothing to do") {
		t.Errorf("outcome %q reads as success over an edge that is still up", got)
	}
	if !strings.Contains(got, "need attention") {
		t.Errorf("outcome %q should point at the resources holding it up", got)
	}
}

// A hostname Purser has never heard of makes the same shape as one that was
// genuinely never stood up, and "nothing to do" is the most reassuring line this
// command can print. It should not be printed over a typo without saying so.
func TestTeardownOutcome_NamesTheNothingItFound(t *testing.T) {
	all := teardownResult(false,
		removal(model.ResourceDNSRecord, spinup.TeardownNone),
		removal(model.ResourceAccessApp, spinup.TeardownNone),
		removal(model.ResourceTunnelRoute, spinup.TeardownNone),
	)
	if got := teardownOutcome(all); !strings.Contains(got, "no record") {
		t.Errorf("outcome %q — a hostname with no rows at all is worth saying outright", got)
	}

	// Whereas everything already removed is a genuine clean bill.
	done := teardownResult(false,
		removal(model.ResourceDNSRecord, spinup.TeardownGone),
		removal(model.ResourceAccessApp, spinup.TeardownGone),
		removal(model.ResourceTunnelRoute, spinup.TeardownNone),
	)
	if got := teardownOutcome(done); !strings.Contains(got, "already gone") {
		t.Errorf("outcome %q", got)
	}
}
