package config

import (
	"strings"
	"testing"
)

func TestLoadBundles_BuiltinsWhenUnset(t *testing.T) {
	bc := loadBundles()
	media, ok := bc.Named["media"]
	if !ok {
		t.Fatalf("expected a built-in media bundle, got %v", bc.Named)
	}
	if strings.Join(media.Services, ",") != "cloudflare,lyceum,argosy" {
		t.Errorf("media bundle: %v", media.Services)
	}
	all, ok := bc.Named["all"]
	if !ok || len(all.Services) != 4 {
		t.Errorf("expected a built-in all bundle with 4 services, got %+v", all)
	}
	// media is the smaller grant, so an unqualified invite can't hand out
	// Switchyard by accident.
	if bc.Default != "media" {
		t.Errorf("default bundle should be media, got %q", bc.Default)
	}
	for _, s := range media.Services {
		if s == "switchyard" {
			t.Error("the media bundle must not include switchyard")
		}
	}
}

func TestLoadBundles_EnvReplacesBuiltins(t *testing.T) {
	t.Setenv("PURSER_BUNDLE_TINY", "argosy")
	bc := loadBundles()
	if _, ok := bc.Named["tiny"]; !ok {
		t.Fatalf("PURSER_BUNDLE_TINY should define a bundle: %v", bc.Named)
	}
	// Defining any bundle replaces the built-in set wholesale — a partial
	// override silently inheriting half a default set is worse than an explicit
	// complete definition.
	if _, ok := bc.Named["media"]; ok {
		t.Error("configuring a bundle should replace the built-ins, not merge with them")
	}
}

func TestLoadBundles_NameIsLowercased(t *testing.T) {
	t.Setenv("PURSER_BUNDLE_FRIENDS", "switchyard,argosy")
	if _, ok := loadBundles().Named["friends"]; !ok {
		t.Error("bundle names should be lowercased for case-insensitive lookup")
	}
}

func TestLoadBundles_ProjectsSuffixAttachesToBundle(t *testing.T) {
	t.Setenv("PURSER_BUNDLE_CREW", "switchyard")
	t.Setenv("PURSER_BUNDLE_CREW_PROJECTS", "IDEA:editor")
	crew := loadBundles().Named["crew"]
	if crew.Projects != "IDEA:editor" {
		t.Errorf("project spec should attach to its bundle, got %q", crew.Projects)
	}
	if strings.Join(crew.Services, ",") != "switchyard" {
		t.Errorf("the _PROJECTS variable must not be parsed as a bundle: %v", crew.Services)
	}
}

func TestLoadBundles_ProjectsForUnknownBundleIsIgnored(t *testing.T) {
	t.Setenv("PURSER_BUNDLE_CREW", "switchyard")
	t.Setenv("PURSER_BUNDLE_GHOST_PROJECTS", "*:admin")
	bc := loadBundles()
	if _, ok := bc.Named["ghost"]; ok {
		t.Error("a _PROJECTS variable must not conjure a bundle with no services")
	}
}

func TestLoadBundles_EmptyValueIgnored(t *testing.T) {
	t.Setenv("PURSER_BUNDLE_EMPTY", "")
	t.Setenv("PURSER_BUNDLE_REAL", "argosy")
	bc := loadBundles()
	if _, ok := bc.Named["empty"]; ok {
		t.Error("an empty bundle definition should be ignored, not registered")
	}
	if _, ok := bc.Named["real"]; !ok {
		t.Error("a valid bundle alongside an empty one should still load")
	}
}

func TestLoadBundles_DefaultOverridable(t *testing.T) {
	t.Setenv("PURSER_DEFAULT_BUNDLE", "all")
	if got := loadBundles().Default; got != "all" {
		t.Errorf("PURSER_DEFAULT_BUNDLE should win, got %q", got)
	}
}

func TestLauncherURL_DerivesFromTeamDomain(t *testing.T) {
	c := CloudflareConfig{TeamDomain: "zero-gravity-industries.cloudflareaccess.com"}
	if got := c.LauncherURL(); got != "https://zero-gravity-industries.cloudflareaccess.com" {
		t.Errorf("LauncherURL() = %q", got)
	}
}

func TestLauncherURL_ExplicitOverrides(t *testing.T) {
	c := CloudflareConfig{
		TeamDomain: "zero-gravity-industries.cloudflareaccess.com",
		Launcher:   "https://launch.example.com/",
	}
	// Trailing slash trimmed so the URL reads cleanly in the credential block.
	if got := c.LauncherURL(); got != "https://launch.example.com" {
		t.Errorf("LauncherURL() = %q", got)
	}
}

// Guard for a zero-value config. Note this is NOT a state Load can produce —
// it gives TeamDomain a hardcoded default — so it protects direct callers only.
func TestLauncherURL_EmptyForZeroValueConfig(t *testing.T) {
	if got := (CloudflareConfig{}).LauncherURL(); got != "" {
		t.Errorf("LauncherURL() = %q, want empty", got)
	}
}

// Load always produces a launcher, which is the flip side of that default:
// there is no "unset" deployment, only a possibly-wrong one.
func TestLauncherURL_LoadAlwaysYieldsOne(t *testing.T) {
	t.Setenv("PURSER_CF_TEAM_DOMAIN", "")
	t.Setenv("PURSER_LAUNCHER_URL", "")
	if got := Load().Cloudflare.LauncherURL(); got == "" {
		t.Error("Load() should still yield a launcher via the TeamDomain default")
	}
}

// A bare host has to become a link — the credential block is plain text, so an
// unclickable string is a dead end for the recipient.
func TestLauncherURL_AddsSchemeToBareHost(t *testing.T) {
	c := CloudflareConfig{Launcher: "launch.example.com"}
	if got := c.LauncherURL(); got != "https://launch.example.com" {
		t.Errorf("LauncherURL() = %q", got)
	}
}

// ...and a value that already carries one must not get a second.
func TestLauncherURL_DoesNotDoublePrefixAScheme(t *testing.T) {
	for _, in := range []string{
		"https://zero-gravity-industries.cloudflareaccess.com",
		"http://launch.internal",
	} {
		c := CloudflareConfig{TeamDomain: in}
		if got := c.LauncherURL(); got != in {
			t.Errorf("LauncherURL() for %q = %q", in, got)
		}
	}
}
