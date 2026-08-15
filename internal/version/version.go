// Package version reports build metadata stamped in at link time.
package version

import (
	"runtime"
	"runtime/debug"
)

// devVersion is what an unstamped build reports before any fallback applies.
const devVersion = "dev"

// unknown is used where a value genuinely cannot be determined, rather than
// inventing something plausible.
const unknown = "unknown"

// Set via -ldflags -X at release time; see .goreleaser.yaml and the Makefile.
var (
	version = devVersion
	commit  = ""
	date    = ""
)

// Info describes the running build.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
}

// Get returns the build metadata for this binary.
func Get() Info {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		bi = nil
	}
	return resolve(version, commit, date, bi)
}

// resolve merges link-time values with whatever the Go toolchain embedded.
// It is separated from Get so the precedence rules can be tested against
// fabricated build info; Get itself only supplies the real inputs.
//
// There are three ways a binary gets here, and they carry different metadata:
//
//   - A release build passes everything via -ldflags. Nothing else is needed.
//   - `go build` inside a checkout embeds vcs.revision, vcs.time and
//     vcs.modified, but no version.
//   - `go install <module>/cmd/binpack@v1.2.3` builds from the module cache,
//     which has no VCS data at all — the module version is the only identity
//     available, in Main.Version.
//
// The third case is easy to miss, and missing it means the most common way to
// install a Go tool reports "dev" with no commit.
func resolve(ldVersion, ldCommit, ldDate string, bi *debug.BuildInfo) Info {
	i := Info{
		Version:   ldVersion,
		Commit:    ldCommit,
		Date:      ldDate,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	if bi != nil {
		// "(devel)" is what a local build reports, and is no more informative
		// than "dev".
		if i.Version == devVersion && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			i.Version = bi.Main.Version
		}

		var modified bool
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if i.Commit == "" {
					i.Commit = s.Value
				}
			case "vcs.time":
				if i.Date == "" {
					i.Date = s.Value
				}
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if modified && i.Commit != "" {
			i.Commit += "-dirty"
		}
	}

	if i.Commit == "" {
		i.Commit = unknown
	}
	if i.Date == "" {
		i.Date = unknown
	}

	return i
}
