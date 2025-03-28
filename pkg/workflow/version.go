package workflow

// Version is the current version of Flow Orchestrator
const Version = "0.1.0-alpha"

// VersionInfo contains detailed version information about the Flow Orchestrator library.
// This can be used by applications to check compatibility and report issues.
var VersionInfo = struct {
	Major      int
	Minor      int
	Patch      int
	PreRelease string
	BuildMeta  string
}{
	Major:      0,
	Minor:      1,
	Patch:      0,
	PreRelease: "alpha",
	BuildMeta:  "",
}
