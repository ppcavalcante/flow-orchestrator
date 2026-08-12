package workflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// goosArchTokens is Go's implicit build-constraint vocabulary: any file whose name ends
// in _<token>.go is compiled ONLY on that GOOS/GOARCH, with no build tag written and no
// diagnostic emitted anywhere.
//
// The list is Go's, not ours, so it is stated in full rather than trimmed to the
// platforms this project runs on. Trimming it would reintroduce the defect on the first
// file someone names _windows_test.go.
var goosArchTokens = map[string]bool{
	// GOOS
	"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true,
	"hurd": true, "illumos": true, "ios": true, "js": true, "linux": true, "nacl": true,
	"netbsd": true, "openbsd": true, "plan9": true, "solaris": true, "wasip1": true,
	"windows": true, "zos": true,
	// GOARCH
	"386": true, "amd64": true, "amd64p32": true, "arm": true, "arm64": true,
	"arm64be": true, "armbe": true, "loong64": true, "mips": true, "mips64": true,
	"mips64le": true, "mips64p32": true, "mips64p32le": true, "mipsle": true,
	"ppc": true, "ppc64": true, "ppc64le": true, "riscv": true, "riscv64": true,
	"s390": true, "s390x": true, "sparc": true, "sparc64": true, "wasm": true,
}

// SEAL-08's FOURTH CLAUSE: no test file may be silently excluded from the suite by its
// own FILENAME.
//
// # This is not hypothetical — it cost a whole test file in this milestone
//
// A seat named a file `..._arm_test.go`. Go reads the trailing `_arm` as an implicit
// GOARCH constraint, so the file compiled on arm and NOWHERE ELSE — on the dev machine
// it simply did not exist. No build tag was written, no warning was printed, and NOTHING
// WE RUN COULD SEE IT: `go build` succeeds, `go vet` succeeds, `go test` reports ok, and
// coverage does not report a file it never compiled. A test that is never compiled is
// indistinguishable from a test that passes.
//
// That is the defect class this whole phase keeps meeting — an instrument that is
// structurally blind to the thing it is supposed to watch — arriving through the
// FILESYSTEM rather than through the code. It is worth a guard precisely because every
// other gate we own is incapable of noticing it.
//
// # What it permits
//
// A deliberate platform-specific test is legitimate. So the rule is not "never use these
// names" but "if you use one, SAY SO": the file must carry an explicit `//go:build`
// constraint. An explicit tag means a human decided; a bare filename means the toolchain
// decided and nobody was told. The explicit tag also makes the intent greppable, which
// the filename convention does not.
//
// # Bite
//
// Creating an empty `zz_probe_arm_test.go` with no build tag reds this with
// "test file(s) silently platform-gated by FILENAME: [zz_probe_arm_test.go (token
// "arm")]". Adding `//go:build arm` to that same file turns it green — proving the
// guard keys on the DECISION being recorded, not on the name being forbidden. Both arms
// were run.
func TestSealed_NoTestFileIsPlatformGatedByItsFilename(t *testing.T) {
	entries, err := filepath.Glob("*_test.go")
	require.NoError(t, err)

	// ANTI-VACUITY. A glob that matched nothing would report no offenders — a clean pass
	// from an instrument looking at an empty set. This package has many times more test
	// files than the floor below, which is deliberately far under the real count so it
	// fails only on a genuinely broken sweep.
	require.Greater(t, len(entries), 50,
		"the sweep found only %d test files; it is BROKEN, not the tree", len(entries))

	var offenders []string
	for _, f := range entries {
		base := strings.TrimSuffix(filepath.Base(f), "_test.go")
		i := strings.LastIndex(base, "_")
		if i < 0 {
			continue
		}
		tok := base[i+1:]
		if !goosArchTokens[tok] {
			continue
		}
		// The name IS platform-gating. That is allowed only if the file says so.
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		if hasBuildConstraint(src) {
			continue
		}
		offenders = append(offenders, filepath.Base(f)+" (token "+strconv.Quote(tok)+")")
	}

	sort.Strings(offenders)
	require.Empty(t, offenders,
		"test file(s) silently platform-gated by FILENAME: %v\n"+
			"Go reads a trailing _<goos>/_<goarch> as an implicit build constraint, so these files "+
			"compile on that platform and NOWHERE ELSE — with no tag written and no diagnostic. "+
			"go build, go vet, go test and coverage are ALL blind to it: a test that never compiles "+
			"looks exactly like a test that passes. Either rename the file, or add an explicit "+
			"//go:build constraint so the exclusion is a recorded decision rather than an accident "+
			"of naming.", offenders)
}

// hasBuildConstraint reports whether src carries a //go:build line in its header — the
// only place Go honours one (before the package clause, followed by a blank line).
// Scanning the header rather than the whole file avoids matching the string inside a
// comment further down, such as the one in this file's own doc block.
func hasBuildConstraint(src []byte) bool {
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//go:build ") || strings.HasPrefix(line, "// +build ") {
			return true
		}
		if strings.HasPrefix(line, "package ") {
			return false
		}
	}
	return false
}

// deliberatelyIgnored is the allow-list for files the toolchain excludes ON PURPOSE.
// Each is excluded by an EXPLICIT constraint its own header carries, which is precisely
// the thing the guard above demands.
var deliberatelyIgnored = map[string]string{
	"go125_off_test.go":          "//go:build !go1.25 — the det-tax calibration pair; its sibling go125_on_test.go compiles here",
	"race_on_test.go":            "//go:build race — compiles only under -race, by design",
	"signal_store_lock_other.go": "platform lock implementation; its sibling compiles on this GOOS",
	// AUD-002 retired the `adversarial_copyclass` tag: DAG.mu became a *sync.RWMutex, a value copy
	// of a DAG no longer trips copylocks, and seal_adversarial_117_copyclass_test.go moved into the
	// default build (no longer ignored). Nothing is allow-listed for that tag anymore.
}

// ARM TWO, and it asks the TOOLCHAIN instead of reimplementing its rules.
//
// The guard above encodes Go's GOOS/GOARCH vocabulary as a literal map, which is a real
// weakness: that list is a copy, and a copy of someone else's rules drifts. `go list -f
// '{{.IgnoredGoFiles}}'` is the authoritative answer — it is Go reporting which files IT
// decided to exclude, by whatever mechanism.
//
// SO WHY KEEP BOTH? Because they fail in opposite directions, and neither subsumes the
// other:
//
//   - This arm is PLATFORM-RELATIVE. It reports what is excluded HERE. On arm64 a file
//     named _arm64_test.go is INCLUDED, so the accident is invisible to this arm on the
//     very machine most likely to make it. Arm one is platform-independent: it reads the
//     name, so it catches _windows_test.go from a Mac.
//   - Arm one only knows FILENAMES. This arm catches exclusion by BUILD TAG too — a
//     stray `//go:build ignore` or a typo'd tag — which no amount of filename inspection
//     can see.
//
// Measured, both directions: dropping an untagged zz_probe_arm_test.go into the package
// makes it appear in IgnoredGoFiles here (verified), and the three entries below are
// genuinely and deliberately excluded on this toolchain.
//
// BITE: removing any entry from deliberatelyIgnored reds with that filename listed as
// silently excluded. Adding an untagged _arm_test.go reds it too — the same seed arm one
// catches, caught a second way, which is the point of having two instruments.
func TestSealed_NoGoFileIsSilentlyExcludedFromTheBuild(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{range .IgnoredGoFiles}}{{.}}\n{{end}}", ".").Output()
	require.NoError(t, err, "go list must run; without it this guard reports nothing and looks green")

	var undeclared []string
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, ok := deliberatelyIgnored[f]; !ok {
			undeclared = append(undeclared, f)
		}
	}

	sort.Strings(undeclared)
	require.Empty(t, undeclared,
		"file(s) excluded from the build without a recorded reason: %v\n"+
			"The Go toolchain is ignoring these — by filename token or by build tag — so any test "+
			"inside them DOES NOT EXIST here, and go build / go vet / go test / coverage all stay "+
			"green regardless. Either fix the constraint, or add the file to deliberatelyIgnored "+
			"with the constraint that justifies it.", undeclared)
}
