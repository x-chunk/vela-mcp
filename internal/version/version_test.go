package version

import (
	"runtime/debug"
	"testing"
)

func TestResolveBuildMetadata(t *testing.T) {
	build := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "embedded-commit"},
			{Key: "vcs.time", Value: "2026-10-14T12:00:00Z"},
		},
	}
	cases := []struct {
		name  string
		input Info
		build *debug.BuildInfo
		want  Info
	}{
		{"linker overrides embedded metadata", Info{Version: "v2.0.0", Commit: "release-commit", Date: "2026-10-13T00:00:00Z"}, build, Info{Version: "v2.0.0", Commit: "release-commit", Date: "2026-10-13T00:00:00Z"}},
		{"go install embeds module version", Info{}, build, Info{Version: "v1.2.3", Commit: "embedded-commit", Date: "2026-10-14T12:00:00Z"}},
		{"development module is not a release", Info{}, &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, Info{Version: "dev", Commit: "none", Date: "unknown"}},
		{"missing build information", Info{}, nil, Info{Version: "dev", Commit: "none", Date: "unknown"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.input, tc.build); got != tc.want {
				t.Fatalf("resolved metadata = %+v, want %+v", got, tc.want)
			}
		})
	}
}
