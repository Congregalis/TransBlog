package version

import (
	"fmt"
	"runtime"
	"testing"
)

func TestStringIncludesGoRuntimeAndPlatform(t *testing.T) {
	oldVersion := Version
	oldCommit := Commit
	oldBuildTime := BuildTime

	Version = "v1.2.3"
	Commit = "abc1234"
	BuildTime = "2026-02-24T20:30:00Z"
	t.Cleanup(func() {
		Version = oldVersion
		Commit = oldCommit
		BuildTime = oldBuildTime
	})

	got := String()
	want := fmt.Sprintf(
		"transblog version=v1.2.3 commit=abc1234 build_time=2026-02-24T20:30:00Z go=%s os=%s arch=%s",
		runtime.Version(),
		runtime.GOOS,
		runtime.GOARCH,
	)
	if got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
