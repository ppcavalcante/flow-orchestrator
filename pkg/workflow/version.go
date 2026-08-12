package workflow

// Version is the current version of Flow Orchestrator.
//
// This reflects the current release tag, v0.22.3-alpha — the final patch on M23
// "Sealed Graph + Complete Mediation" (v0.22.0-alpha), completing a fully green amd64
// CI. It carries the v0.22.1-alpha det-tax root-cause fix (fmt.Sprintf removed from
// DAG.Execute's hot loop), the v0.22.2-alpha internal/workflow/memory coverage
// restoration, and a CI-infra fix dropping the redundant -race from the coverage-
// generation targets (race is separately gated; it was doubling the run time into the
// 30m timeout). The next milestone's work is documented under [Unreleased] in
// CHANGELOG.md and is bumped to its own tag only when it actually ships.
const Version = "0.22.3-alpha"

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
	Patch:      3,
	PreRelease: "alpha",
	BuildMeta:  "",
}
