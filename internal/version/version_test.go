package version

import (
	"strings"
	"testing"
)

func TestResolvePrefersLdflagsValues(t *testing.T) {
	version, commit, buildDate := Resolve("v1.2.3", "abc1234", "2026-08-22T00:00:00Z")
	if version != "v1.2.3" {
		t.Errorf("expected version v1.2.3, got %s", version)
	}
	if commit != "abc1234" {
		t.Errorf("expected commit abc1234, got %s", commit)
	}
	if buildDate != "2026-08-22T00:00:00Z" {
		t.Errorf("expected buildDate 2026-08-22T00:00:00Z, got %s", buildDate)
	}
}

func TestResolveFallsBackWhenEmpty(t *testing.T) {
	version, commit, buildDate := Resolve("", "", "")
	if version == "" {
		t.Error("Resolve should never return an empty version")
	}
	if commit == "" {
		t.Error("Resolve should never return an empty commit")
	}
	if buildDate == "" {
		t.Error("Resolve should never return an empty buildDate")
	}
}

func TestInfo(t *testing.T) {
	info := Info("v1.2.3", "abc1234", "2026-08-22T00:00:00Z")
	if !strings.Contains(info, "sendspin-voip version v1.2.3") {
		t.Errorf("unexpected Info() format: %s", info)
	}
	if !strings.Contains(info, "abc1234") || !strings.Contains(info, "2026-08-22T00:00:00Z") {
		t.Errorf("Info() should contain commit and buildDate, got %s", info)
	}
}
