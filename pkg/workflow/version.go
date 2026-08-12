package workflow

// Version is the current version of Flow Orchestrator.
//
// This reflects the current release tag, v0.22.4-alpha — the M23 "Sealed Graph +
// Complete Mediation" (v0.22.0-alpha) release with a fully green amd64 CI, rollup
// included. The v0.22.1–v0.22.3 patches fixed the det-tax load-flake root cause
// (fmt.Sprintf out of DAG.Execute's hot loop), restored internal/workflow/memory
// coverage, and dropped the redundant -race from the coverage-generation targets;
// v0.22.4 makes the informational mutation job self-terminate via a shell timeout so
// its budget cap no longer cancels (and taints) the workflow rollup. The next
// milestone's work is documented under [Unreleased] in CHANGELOG.md and is bumped to
// its own tag only when it actually ships.
const Version = "0.22.4-alpha"

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
	Patch:      4,
	PreRelease: "alpha",
	BuildMeta:  "",
}
