package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestFill(t *testing.T) {
	vcs := []debug.BuildSetting{
		{Key: "vcs.revision", Value: "bbce502f67e27cc9bd0082ffe64fa033d98b163d"},
		{Key: "vcs.time", Value: "2026-10-15T08:44:19Z"},
	}
	tests := []struct {
		name  string
		start Info
		bi    *debug.BuildInfo
		want  Info
	}{
		{
			name:  "release values win over embedded ones",
			start: Info{Version: "v0.0.1", Commit: "abc1234", Date: "2026-10-20T00:00:00Z"},
			bi:    &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}, Settings: vcs},
			want:  Info{Version: "v0.0.1", Commit: "abc1234", Date: "2026-10-20T00:00:00Z"},
		},
		{
			name: "go install uses the module version and vcs data",
			bi:   &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}, Settings: vcs},
			want: Info{Version: "v0.1.0", Commit: "bbce502f67e2", Date: "2026-10-15T08:44:19Z"},
		},
		{
			name: "local build without version info",
			bi:   &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			want: Info{Version: "dev", Commit: "unknown", Date: "unknown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.start
			fillFromBuildInfo(&got, tt.bi)
			fillDefaults(&got)
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGet(t *testing.T) {
	info := Get()
	if info.Version == "" || info.Commit == "" || info.Date == "" || info.Go == "" || info.Platform == "" {
		t.Errorf("Get() left a field empty: %+v", info)
	}
}
