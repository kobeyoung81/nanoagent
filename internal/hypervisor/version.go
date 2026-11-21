package hypervisor

import (
	"os"
	"strings"
)

// Version is the release tag this binary was built from, stamped at build time
// with -X. /healthz reports it so an operator can tell which build a container
// runs without inspecting image labels.
var Version = "dev"

// Platform reports the distribution this container runs on, read from
// /etc/os-release, as "ubuntu 24.04".
//
// It exists because agents must run on glibc (they install apt packages, run
// .NET through officecli, load python wheels and node native modules), while
// the services run on musl/Alpine. Answering "which one is this container"
// used to mean entering it and reading /etc/os-release by hand; `nanosec
// version` and /healthz report it now.
func Platform() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "unknown"
	}
	id, versionID := "", ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ID="):
			id = strings.Trim(strings.TrimPrefix(line, "ID="), `"'`)
		case strings.HasPrefix(line, "VERSION_ID="):
			versionID = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), `"'`)
		}
	}
	return strings.TrimSpace(id + " " + versionID)
}
