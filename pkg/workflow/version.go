package workflow

// Version is the current version of Flow Orchestrator.
//
// This reflects the current release tag, v0.22.0-alpha — M23 "Sealed Graph +
// Complete Mediation" plus the independent-audit remediation and dogfood/hardening
// pass. The next milestone's work is documented under [Unreleased] in CHANGELOG.md
// and is bumped to its own tag only when it actually ships.
const Version = "0.22.0-alpha"

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
	Patch:      0,
	PreRelease: "alpha",
	BuildMeta:  "",
}
