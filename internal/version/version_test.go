package version

import (
	"runtime/debug"
	"testing"
)

func buildInfo(mainVersion string, settings map[string]string) *debug.BuildInfo {
	bi := &debug.BuildInfo{Main: debug.Module{Version: mainVersion}}
	for k, v := range settings {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: k, Value: v})
	}
	return bi
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name                    string
		ldVersion, ldCommit     string
		ldDate                  string
		bi                      *debug.BuildInfo
		wantVersion, wantCommit string
		wantDate                string
	}{
		{
			name:      "release build: ldflags win outright",
			ldVersion: "v1.2.3", ldCommit: "abc123", ldDate: "2026-08-15T00:00:00Z",
			bi:          buildInfo("v9.9.9", map[string]string{"vcs.revision": "ignored"}),
			wantVersion: "v1.2.3", wantCommit: "abc123", wantDate: "2026-08-15T00:00:00Z",
		},
		{
			// A stamped commit still gets marked dirty: the binary genuinely
			// does not correspond to that commit, and a local `make build`
			// with uncommitted changes should say so.
			name:      "stamped commit in a dirty tree is still marked",
			ldVersion: "v1.2.3", ldCommit: "abc123",
			bi:          buildInfo("", map[string]string{"vcs.modified": "true"}),
			wantVersion: "v1.2.3", wantCommit: "abc123-dirty", wantDate: unknown,
		},
		{
			name:      "go install from module cache: version comes from Main",
			ldVersion: devVersion,
			bi:        buildInfo("v0.4.0", nil),
			// No VCS data exists in the module cache, so commit and date are
			// honestly unknown rather than guessed.
			wantVersion: "v0.4.0", wantCommit: unknown, wantDate: unknown,
		},
		{
			name:      "go build in a checkout: VCS data, no version",
			ldVersion: devVersion,
			bi: buildInfo("(devel)", map[string]string{
				"vcs.revision": "deadbeef",
				"vcs.time":     "2026-08-15T11:00:00Z",
			}),
			wantVersion: devVersion, wantCommit: "deadbeef", wantDate: "2026-08-15T11:00:00Z",
		},
		{
			name:      "dirty checkout is marked",
			ldVersion: devVersion,
			bi: buildInfo("(devel)", map[string]string{
				"vcs.revision": "deadbeef",
				"vcs.modified": "true",
			}),
			wantVersion: devVersion, wantCommit: "deadbeef-dirty", wantDate: unknown,
		},
		{
			name:        "no build info at all",
			ldVersion:   devVersion,
			bi:          nil,
			wantVersion: devVersion, wantCommit: unknown, wantDate: unknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(tc.ldVersion, tc.ldCommit, tc.ldDate, tc.bi)

			if got.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", got.Version, tc.wantVersion)
			}
			if got.Commit != tc.wantCommit {
				t.Errorf("Commit = %q, want %q", got.Commit, tc.wantCommit)
			}
			if got.Date != tc.wantDate {
				t.Errorf("Date = %q, want %q", got.Date, tc.wantDate)
			}
			if got.GoVersion == "" || got.Platform == "" {
				t.Errorf("GoVersion and Platform must always be populated, got %+v", got)
			}
		})
	}
}

func TestGetNeverReturnsEmptyFields(t *testing.T) {
	got := Get()
	if got.Version == "" || got.Commit == "" || got.Date == "" ||
		got.GoVersion == "" || got.Platform == "" {
		t.Errorf("no field may render empty, got %+v", got)
	}
}
