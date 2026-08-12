package workflow

import (
	"fmt"
	"regexp"
	"testing"
)

// CUR-007 / AUD-009 guard: Version (the string) and VersionInfo (the struct) are
// two encodings of the SAME fact and must never silently drift. The real prior
// bug was VersionInfo.Minor == 20 while Version == "0.21.0-alpha". This test
// composes VersionInfo back into a semver string and asserts byte-equality with
// Version, so any future edit that touches one without the other reds the build.
//
// It runs in every `go test ./...` job automatically — no CI wiring required.

// semverish is the exact regex the release preflight applies to the tag-derived
// version. Keeping it identical here means the always-run test and the release
// gate agree on what "well-formed" means.
var semverish = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

func TestVersionInfoComposesToVersion(t *testing.T) {
	got := fmt.Sprintf("%d.%d.%d", VersionInfo.Major, VersionInfo.Minor, VersionInfo.Patch)
	if VersionInfo.PreRelease != "" {
		got += "-" + VersionInfo.PreRelease
	}
	if VersionInfo.BuildMeta != "" {
		got += "+" + VersionInfo.BuildMeta
	}
	if got != Version {
		t.Fatalf("VersionInfo drifted from Version (the AUD-009 bug):\n"+
			"  composed VersionInfo = %q\n"+
			"  const Version        = %q\n"+
			"fix pkg/workflow/version.go so the struct fields and the string agree",
			got, Version)
	}
}

func TestVersionIsSemverish(t *testing.T) {
	if !semverish.MatchString(Version) {
		t.Fatalf("Version %q does not match semver-ish %s", Version, semverish.String())
	}
}
