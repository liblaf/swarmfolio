package buildinfo

import "runtime/debug"

// Version is set by the release build. Development builds derive it from Go's
// embedded VCS metadata when available.
var Version = "dev"

func Current() string {
	if Version != "dev" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return Version
}
