package version

import "fmt"

var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)

func String() string {
	return fmt.Sprintf("transblog version=%s commit=%s build_time=%s", Version, Commit, BuildTime)
}
