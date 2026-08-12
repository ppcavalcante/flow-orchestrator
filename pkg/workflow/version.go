package workflow

// Version is the current version of Flow Orchestrator.
//
// This reflects the current release tag, v0.22.1-alpha — a patch on M23 "Sealed
// Graph + Complete Mediation" (v0.22.0-alpha) that fixes the det-tax gate's
// load-dependent false-red at its root (fmt.Sprintf removed from DAG.Execute's hot
// loop, eliminating a GC-drained sync.Pool allocation). The next milestone's work is
// documented under [Unreleased] in CHANGELOG.md and is bumped to its own tag only
// when it actually ships.
const Version = "0.22.1-alpha"

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
	Minor:      22, // kept in sync with Version above
	Patch:      1,
	PreRelease: "alpha",
	BuildMeta:  "",
}
