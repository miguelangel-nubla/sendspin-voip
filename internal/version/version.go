package version

import (
	"cmp"
	"runtime/debug"
)

const (
	// Product is the reported product name.
	Product = "Sendspin VoIP Bridge"
	// Manufacturer is the reported manufacturer.
	Manufacturer = "miguelangel-nubla"
)

// Resolve returns the effective version, commit, and build date. The
// ldflags-injected values (set via -X main.Version=... at build time) take
// priority; any left empty fall back to the VCS info the Go toolchain embeds
// automatically (go run/go install/plain go build without ldflags).
func Resolve(ldflagsVersion, ldflagsCommit, ldflagsBuildDate string) (version, commit, buildDate string) {
	version, commit, buildDate = "dev", "none", "unknown"

	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			version = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				commit = setting.Value
			case "vcs.time":
				buildDate = setting.Value
			}
		}
	}

	return cmp.Or(ldflagsVersion, version), cmp.Or(ldflagsCommit, commit), cmp.Or(ldflagsBuildDate, buildDate)
}

// Info formats build and version details for display.
func Info(version, commit, buildDate string) string {
	return "sendspin-voip version " + version + " (commit: " + commit + ", built: " + buildDate + ")"
}
