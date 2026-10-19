// Package buildinfo reports which version of stuntdb is running.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Release builds set these with
// -ldflags "-X github.com/exarth-labs/stuntdb/internal/buildinfo.version=v1.2.3 ...".
var (
	version string
	commit  string
	date    string
)

// Info describes a stuntdb build.
type Info struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
	Go       string `json:"go"`
	Platform string `json:"platform"`
}

// Get returns the build information. Values set by the release build win;
// otherwise it uses what the Go toolchain embedded in the binary.
func Get() Info {
	info := Info{
		Version:  version,
		Commit:   commit,
		Date:     date,
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		fillFromBuildInfo(&info, bi)
	}
	fillDefaults(&info)
	return info
}

func fillFromBuildInfo(info *Info, bi *debug.BuildInfo) {
	if info.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		info.Version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "" {
				info.Commit = shortCommit(s.Value)
			}
		case "vcs.time":
			if info.Date == "" {
				info.Date = s.Value
			}
		}
	}
}

func fillDefaults(info *Info) {
	if info.Version == "" {
		info.Version = "dev"
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	if info.Date == "" {
		info.Date = "unknown"
	}
}

func shortCommit(rev string) string {
	const n = 12
	if len(rev) > n {
		return rev[:n]
	}
	return rev
}
