package version

import (
	"fmt"
	"runtime"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildTime = "unknown"
)

func String() string {
	return fmt.Sprintf(
		"transblog version=%s commit=%s build_time=%s go=%s os=%s arch=%s",
		Version,
		Commit,
		BuildTime,
		runtime.Version(),
		runtime.GOOS,
		runtime.GOARCH,
	)
}
