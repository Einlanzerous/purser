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
