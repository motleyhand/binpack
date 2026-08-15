// Package version reports build metadata stamped in at link time.
package version

import (
	"runtime"
	"runtime/debug"
)

// Set via -ldflags -X at release time; see .goreleaser.yaml.
// The defaults are what a plain `go build` produces.
var (
	version = "dev"
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

// Get returns the build metadata, falling back to what the Go toolchain
// embedded when ldflags were not supplied. A `go install` of this module
// records the VCS revision automatically, so an unstamped build is still
// traceable to a commit.
func Get() Info {
	i := Info{
		Version:   version,
		Commit:    commit,
		Date:      date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	if bi, ok := debug.ReadBuildInfo(); ok {
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
			}
		}
	}

	if i.Commit == "" {
		i.Commit = "unknown"
	}
	if i.Date == "" {
		i.Date = "unknown"
	}

	return i
}
