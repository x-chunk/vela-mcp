// Package version resolves release metadata for the CLI and MCP handshake.
// Release builds inject Version, Commit and Date with linker flags. A plain
// go install falls back to module and VCS metadata embedded by the Go toolchain.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// These strings must remain variables because go build -ldflags -X only
// overrides package-level string variables.
var (
	Version = ""
	Commit  = ""
	Date    = ""
)

// Info identifies the binary and the toolchain and platform that built it.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get prefers linker values, then Go build information, then explicit
// development defaults. No current time is used, keeping rebuilds reproducible.
func Get() Info {
	info := Info{
		Version: Version, Commit: Commit, Date: Date,
		GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	build, _ := debug.ReadBuildInfo()
	return resolve(info, build)
}

func resolve(info Info, build *debug.BuildInfo) Info {
	if build != nil {
		if info.Version == "" && build.Main.Version != "" && build.Main.Version != "(devel)" {
			info.Version = build.Main.Version
		}
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = setting.Value
				}
			case "vcs.time":
				if info.Date == "" {
					info.Date = setting.Value
				}
			}
		}
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	if info.Commit == "" {
		info.Commit = "none"
	}
	if info.Date == "" {
		info.Date = "unknown"
	}
	return info
}

// String returns one line suitable for vela-mcp -version and support reports.
func (i Info) String() string {
	commit := i.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	return fmt.Sprintf("%s (commit %s, built %s, %s, %s)", i.Version, commit, i.Date, i.GoVersion, i.Platform)
}
