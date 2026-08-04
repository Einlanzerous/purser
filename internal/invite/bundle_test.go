package invite

import (
	"context"
	"strings"
	"testing"

	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/model"
)

func testBundles() BundleSet {
	return BundleSet{
		Named: map[string]Bundle{
			"media": {Services: []string{"cloudflare", "lyceum", "argosy"}},
			"all":   {Services: []string{"cloudflare", "switchyard", "lyceum", "argosy"}},
			"scoped": {Services: []string{"switchyard"},
				Projects: "IDEA:editor"},
		},
		Default: "media",
	}
}

func TestResolveServices_ExplicitOnly(t *testing.T) {
	got, applied, err := testBundles().resolveServices([]string{"argosy"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "argosy" {
		t.Errorf("explicit --to should pass through untouched: %v", got)
	}
	if applied != "" {
		t.Errorf("an explicit list came from no bundle, got %q", applied)
	}
}

func TestResolveServices_BundleOnly(t *testing.T) {
	got, applied, err := testBundles().resolveServices(nil, "all")
	if err != nil {
		t.Fatal(err)
	}
	want := "cloudflare,switchyard,lyceum,argosy"
	if strings.Join(got, ",") != want {
		t.Errorf("want %s, got %s", want, strings.Join(got, ","))
	}
	if applied != "all" {
		t.Errorf("applied bundle: %q", applied)
	}
}

func TestResolveServices_NeitherUsesDefault(t *testing.T) {
	got, applied, err := testBundles().resolveServices(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "cloudflare,lyceum,argosy" {
		t.Errorf("default bundle should be media, got %v", got)
	}
	if applied != "media" {
		t.Errorf("applied bundle should be reported as the default: %q", applied)
	}
}

func TestResolveServices_UnionDedupesAndKeepsOrder(t *testing.T) {
	// Bundle first, then extras — and "argosy" already in the bundle must not
	// appear twice, or the credential block would list it twice.
	got, _, err := testBundles().resolveServices([]string{"argosy", "switchyard"}, "media")
	if err != nil {
		t.Fatal(err)
	}
	want := "cloudflare,lyceum,argosy,switchyard"
	if strings.Join(got, ",") != want {
		t.Errorf("want %s, got %s", want, strings.Join(got, ","))
	}
}

func TestResolveServices_CaseInsensitiveBundle(t *testing.T) {
	if _, _, err := testBundles().resolveServices(nil, "  ALL  "); err != nil {
		t.Errorf("bundle names should match case/space-insensitively: %v", err)
	}
}

func TestResolveServices_UnknownBundleListsKnown(t *testing.T) {
	_, _, err := testBundles().resolveServices(nil, "nope")
	if err == nil {
		t.Fatal("expected an error for an unknown bundle")
	}
	// The message has to name the alternatives or the operator is left guessing.
	for _, want := range []string{"nope", "all", "media"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

func TestResolveServices_NoDefaultConfigured(t *testing.T) {
	empty := BundleSet{}
	_, _, err := empty.resolveServices(nil, "")
	if err == nil {
		t.Fatal("expected an error when nothing is given and no default exists")
	}
	if !strings.Contains(err.Error(), "PURSER_DEFAULT_BUNDLE") {
		t.Errorf("error should point at the fix: %v", err)
	}
}

func TestResolveServices_DefaultBundleUndefined(t *testing.T) {
	bs := BundleSet{Named: map[string]Bundle{"media": {Services: []string{"argosy"}}}, Default: "ghost"}
	_, _, err := bs.resolveServices(nil, "")
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("a default naming a missing bundle should say so: %v", err)
	}
}

func TestProjectsFor_BundleDefaultsToUserLevel(t *testing.T) {
	grants, err := testBundles().projectsFor("all", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Key != "*" || grants[0].Role != "user" {
		t.Errorf("a bundle with no project spec should grant %s, got %+v", DefaultProjectGrant, grants)
	}
}

func TestProjectsFor_BundleSpecWins(t *testing.T) {
	grants, err := testBundles().projectsFor("scoped", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Key != "IDEA" || grants[0].Role != "editor" {
		t.Errorf("the bundle's own spec should apply: %+v", grants)
	}
}

func TestProjectsFor_ExplicitBeatsBundle(t *testing.T) {
	explicit, err := ParseProjectGrants("SWY:admin")
	if err != nil {
		t.Fatal(err)
	}
	grants, err := testBundles().projectsFor("all", explicit)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 || grants[0].Role != "admin" {
		t.Errorf("an explicit --projects must override the bundle default: %+v", grants)
	}
}

func TestProjectsFor_NoBundleMeansNoDefaultGrant(t *testing.T) {
	// An explicit --to invite shouldn't silently acquire project grants it
	// never asked for.
	grants, err := testBundles().projectsFor("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Errorf("explicit invites get no implicit grants: %+v", grants)
	}
}

func TestParseProjectGrants_MalformedIsAnError(t *testing.T) {
	// A silent skip would provision the person at the wrong level.
	if _, err := ParseProjectGrants("*:user,garbage"); err == nil {
		t.Fatal("expected an error for a malformed grant")
	}
	if _, err := ParseProjectGrants("*:user, IDEA:editor "); err != nil {
		t.Errorf("whitespace should be tolerated: %v", err)
	}
}

func TestBundleSet_Names(t *testing.T) {
	got := strings.Join(testBundles().Names(), ",")
	if got != "all,media,scoped" {
		t.Errorf("Names should be sorted for stable help/error output: %s", got)
	}
}

// End-to-end: an invite naming neither services nor a bundle provisions the
// default bundle's services, and the Switchyard connector receives the
// user-level project grant.
func TestRun_DefaultBundle_ProvisionsSetAndGrantsUserLevel(t *testing.T) {
	st := newFakeStore()
	cf := &fakeConn{key: "cloudflare", display: "Cloudflare Access (SSO)"}
	ly := &fakeConn{key: "lyceum", display: "Lyceum"}
	ar := &fakeConn{key: "argosy", display: "Argosy"}
	sw := &fakeConn{key: "switchyard", display: "Switchyard"}
	reg := connector.NewRegistry(cf, ly, ar, sw)

	svc := New(seededStore(t, st, reg), reg, nil, WithBundles(testBundles()))
	res, err := svc.Run(context.Background(), Request{
		Name: "Ada", Email: "ada@example.com", Delivery: model.DeliverCopyPaste,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Bundle != "media" {
		t.Errorf("result should report the applied default bundle, got %q", res.Bundle)
	}
	for _, c := range []*fakeConn{cf, ly, ar} {
		if c.callCount() != 1 {
			t.Errorf("%s should have been provisioned once, got %d", c.key, c.callCount())
		}
	}
	// media deliberately excludes Switchyard — that's the whole point of a
	// non-technical bundle.
	if sw.callCount() != 0 {
		t.Errorf("switchyard is not in the media bundle, but was provisioned")
	}
}

func TestRun_AllBundle_PassesUserProjectGrantToSwitchyard(t *testing.T) {
	st := newFakeStore()
	cf := &fakeConn{key: "cloudflare", display: "Cloudflare Access (SSO)"}
	ly := &fakeConn{key: "lyceum", display: "Lyceum"}
	ar := &fakeConn{key: "argosy", display: "Argosy"}
	sw := &fakeConn{key: "switchyard", display: "Switchyard"}
	reg := connector.NewRegistry(cf, ly, ar, sw)

	svc := New(seededStore(t, st, reg), reg, nil, WithBundles(testBundles()))
	if _, err := svc.Run(context.Background(), Request{
		Name: "Ada", Email: "ada@example.com", Bundle: "all", Delivery: model.DeliverCopyPaste,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	grants := sw.lastInput().Projects
	if len(grants) != 1 || grants[0].Key != "*" || grants[0].Role != DefaultProjectRole {
		t.Errorf("bundle invites should grant %s on Switchyard, got %+v", DefaultProjectGrant, grants)
	}
	// The instance role stays at the preset default — a bundle sets project
	// access, not instance-wide privilege.
	if role := sw.lastInput().InstanceRole; role != "" {
		t.Errorf("a bundle must not escalate the instance role, got %q", role)
	}
}

func TestRun_UnknownBundle_FailsBeforeAnyProvisioning(t *testing.T) {
	st := newFakeStore()
	ar := &fakeConn{key: "argosy", display: "Argosy"}
	reg := connector.NewRegistry(ar)

	svc := New(seededStore(t, st, reg), reg, nil, WithBundles(testBundles()))
	_, err := svc.Run(context.Background(), Request{
		Name: "Ada", Email: "ada@example.com", Bundle: "nope", Delivery: model.DeliverCopyPaste,
	})
	if err == nil {
		t.Fatal("expected an unknown bundle to fail")
	}
	if ar.callCount() != 0 {
		t.Errorf("nothing should be provisioned when the bundle is invalid, got %d calls", ar.callCount())
	}
}

func TestRun_BundleNamingUnregisteredService_IsRejected(t *testing.T) {
	st := newFakeStore()
	ar := &fakeConn{key: "argosy", display: "Argosy"}
	reg := connector.NewRegistry(ar) // no cloudflare/lyceum registered

	svc := New(seededStore(t, st, reg), reg, nil, WithBundles(testBundles()))
	err := svc.Validate(Request{Name: "Ada", Email: "ada@example.com", Bundle: "media"})
	if err == nil {
		t.Fatal("a bundle naming an unregistered service should fail validation")
	}
	if !strings.Contains(err.Error(), "media") {
		t.Errorf("the error should name the offending bundle: %v", err)
	}
}
