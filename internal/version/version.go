// Package version holds the build's version stamp. The release build (Makefile LDFLAGS,
// Dockerfile, .goreleaser.yaml) sets the variables with
//
//	-ldflags "-X github.com/chenhunghan/flare-operator/internal/version.Version=v0.1.0
//	          -X github.com/chenhunghan/flare-operator/internal/version.Commit=<sha>
//	          -X github.com/chenhunghan/flare-operator/internal/version.Date=<RFC 3339>"
//
// A plain `go build` or `go run` leaves Version "dev"; Commit and Date then fall back to the
// VCS stamp that the go command embeds (runtime/debug.BuildInfo), when there is one.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Set by -ldflags -X at link time.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is the version stamp of the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
	// Modified is true when the VCS stamp says the working tree had uncommitted changes.
	Modified bool `json:"modified,omitempty"`
}

// Get returns the version stamp: the -ldflags values, completed from the go command's VCS
// build settings when the linker left them empty.
func Get() Info {
	return get(debug.ReadBuildInfo)
}

func get(read func() (*debug.BuildInfo, bool)) Info {
	in := Info{Version: Version, Commit: Commit, Date: Date, GoVersion: runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if bi, ok := read(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if in.Commit == "" {
					in.Commit = s.Value
				}
			case "vcs.time":
				if in.Date == "" {
					in.Date = s.Value
				}
			case "vcs.modified":
				in.Modified = s.Value == "true"
			}
		}
	}
	if in.Commit == "" {
		in.Commit = "unknown"
	}
	if in.Date == "" {
		in.Date = "unknown"
	}
	return in
}

// String is the one-line form printed by --version, e.g.
// "flare-operator v0.1.0 (commit 6fa76a7, built 2026-09-29T12:00:00Z, go1.26.8 linux/arm64)".
func (i Info) String(program string) string {
	commit := i.Commit
	if i.Modified {
		commit += "-dirty"
	}
	return fmt.Sprintf("%s %s (commit %s, built %s, %s %s)", program, i.Version, commit, i.Date, i.GoVersion, i.Platform)
}

// KeysAndValues returns the stamp as logr key/value pairs for a startup log line.
func (i Info) KeysAndValues() []any {
	return []any{"version", i.Version, "commit", i.Commit, "buildDate", i.Date, "goVersion", i.GoVersion, "platform", i.Platform}
}
