package workflow

// Version is the current version of Flow Orchestrator.
//
// This reflects the current release tag, v0.22.2-alpha — a patch on M23 "Sealed
// Graph + Complete Mediation" (v0.22.0-alpha). It carries the v0.22.1-alpha det-tax
// root-cause fix (fmt.Sprintf removed from DAG.Execute's hot loop) plus the closure
// of a pre-existing coverage gap M23 opened in internal/workflow/memory (node_pool.go
// deletion dropped the package below its floor; buffer_pool.go now fully tested). The
// next milestone's work is documented under [Unreleased] in CHANGELOG.md and is
// bumped to its own tag only when it actually ships.
const Version = "0.22.2-alpha"

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
	Patch:      2,
	PreRelease: "alpha",
	BuildMeta:  "",
}
