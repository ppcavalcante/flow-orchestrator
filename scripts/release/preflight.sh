#!/usr/bin/env bash
# CUR-007: Release preflight — bind tag, source version, changelog, prior-release
# ancestry, and the blocking-gate manifest to ONE candidate SHA before a tag is
# pushed. Nothing here mutates the repo (except a self-cleaning temp dir under
# RUN_GATES=1). Every check fails LOUD and non-zero with a per-check message.
#
# Usage:   scripts/release/preflight.sh v0.22.0-alpha
#
# Env:
#   RUN_GATES=1                         also run the cheap gates (build, vet,
#                                       gofmt -l, generated-code freshness)
#   RELEASE_ALLOW_REPLACEMENT_LINEAGE=1 permit a candidate that does NOT descend
#                                       from the previous tag, IFF a tracked ADR
#                                       under docs/architecture/adr/ documents the
#                                       intentional replacement lineage.
set -euo pipefail

fail() { printf 'PREFLIGHT FAIL [%s]: %s\n' "$1" "$2" >&2; exit 1; }
ok()   { printf 'PREFLIGHT  OK  [%s]: %s\n' "$1" "$2"; }

TAG="${1:-}"
[ -n "$TAG" ] || fail "args" "missing candidate tag. usage: $0 v0.22.0-alpha"

# Resolve repo root so the script is safe to run from anywhere.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

VERSION_GO="pkg/workflow/version.go"
CHANGELOG="CHANGELOG.md"
ADR_DIR="docs/architecture/adr"

# ── 1. tag <-> version ───────────────────────────────────────────────────────
# Strip a single leading 'v'; the remainder must equal the const Version string.
TAG_VERSION="${TAG#v}"
[ -f "$VERSION_GO" ] || fail "tag<->version" "$VERSION_GO not found"
# Parse `const Version = "..."` WITHOUT compiling/running the package.
SRC_VERSION="$(sed -nE 's/^[[:space:]]*const[[:space:]]+Version[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "$VERSION_GO" | head -1)"
[ -n "$SRC_VERSION" ] || fail "tag<->version" "could not parse const Version from $VERSION_GO"
if [ "$TAG_VERSION" != "$SRC_VERSION" ]; then
	fail "tag<->version" "tag '$TAG' (version '$TAG_VERSION') != pkg/workflow.Version '$SRC_VERSION'"
fi
ok "tag<->version" "$TAG matches pkg/workflow.Version = $SRC_VERSION"

# ── 2. changelog ─────────────────────────────────────────────────────────────
# Require a RELEASED section header for this version with a date on the same line.
# Accept ASCII hyphen or en/em dash as the separator (repo convention uses em-dash).
[ -f "$CHANGELOG" ] || fail "changelog" "$CHANGELOG not found"
CL_LINE="$(grep -nE "^## \[${SRC_VERSION//./\\.}\][[:space:]]*[-–—][[:space:]]*[0-9]{4}-[0-9]{2}-[0-9]{2}" "$CHANGELOG" || true)"
if [ -z "$CL_LINE" ]; then
	fail "changelog" "no released section '## [$SRC_VERSION] — YYYY-MM-DD' in $CHANGELOG (an [Unreleased] entry does NOT count)"
fi
ok "changelog" "matched -> ${CL_LINE}"

# ── 3. ancestry ──────────────────────────────────────────────────────────────
# HEAD must descend from the previous published tag (excluding the candidate).
PREV_TAG="$(git tag --sort=-v:refname | grep -v "^${TAG}$" | head -1 || true)"
if [ -z "$PREV_TAG" ]; then
	ok "ancestry" "no previous published tag found — treating as first release (skipping descent check)"
elif git merge-base --is-ancestor "$PREV_TAG" HEAD; then
	ok "ancestry" "HEAD descends from previous tag $PREV_TAG"
else
	if [ "${RELEASE_ALLOW_REPLACEMENT_LINEAGE:-0}" = "1" ]; then
		# Override permitted ONLY with a tracked ADR documenting the replacement lineage.
		if [ -d "$ADR_DIR" ] && ls "$ADR_DIR"/*.md >/dev/null 2>&1; then
			printf 'PREFLIGHT WARN [ancestry]: HEAD does NOT descend from %s — OVERRIDDEN via RELEASE_ALLOW_REPLACEMENT_LINEAGE=1.\n' "$PREV_TAG" >&2
			printf 'PREFLIGHT WARN [ancestry]: this REQUIRES a tracked ADR under %s/ documenting the intentional replacement lineage / AUD-009 reclassification. Confirm it exists and is accurate before tagging.\n' "$ADR_DIR" >&2
			ok "ancestry" "replacement lineage accepted under ADR requirement (previous tag $PREV_TAG)"
		else
			fail "ancestry" "override requested but no ADR present under $ADR_DIR/ documenting the replacement lineage"
		fi
	else
		fail "ancestry" "HEAD does NOT descend from previous tag $PREV_TAG (set RELEASE_ALLOW_REPLACEMENT_LINEAGE=1 + a tracked ADR to intentionally replace lineage)"
	fi
fi

# ── 4. blocking-gate manifest ────────────────────────────────────────────────
# The heavy gates run in CI, bound to THIS SHA. Print the authoritative list so
# the operator/CI can bind them to the candidate commit.
CANDIDATE_SHA="$(git rev-parse HEAD)"
cat <<EOF

Blocking gates that MUST pass on candidate SHA ${CANDIDATE_SHA} for ${TAG}:
  1.  build:                    GOTOOLCHAIN=local go build ./...
  2.  vet:                      GOTOOLCHAIN=local go vet ./...
  3.  race test suite:          GOTOOLCHAIN=local go test -race -timeout 30m ./...
  4.  lint:                     golangci-lint run
  5.  vulnerabilities:          govulncheck ./...
  6.  formal TLC capstones:     the specs/ TLC model-check capstones
  7.  generated-code freshness: regenerate FlatBuffers -> zero git diff
  8.  docs gate:                docs verification gate
  9.  examples run:             examples actually execute (not just build)
  10. coverage:                 total coverage >= 90%
EOF

# ── 5. cheap gates (opt-in) ──────────────────────────────────────────────────
if [ "${RUN_GATES:-0}" = "1" ]; then
	GO="${GO:-go}"
	printf '\nRUN_GATES=1: running cheap gates...\n'

	printf -- '- build...\n'
	GOTOOLCHAIN=local "$GO" build ./... || fail "gate:build" "go build ./... failed"
	ok "gate:build" "go build ./... clean"

	printf -- '- vet...\n'
	GOTOOLCHAIN=local "$GO" vet ./... || fail "gate:vet" "go vet ./... failed"
	ok "gate:vet" "go vet ./... clean"

	printf -- '- gofmt...\n'
	# Only tracked .go files — a raw `gofmt -l .` walk ignores .gitignore and
	# would flag gitignored scratch trees (_local/, agent worktrees under
	# .claude/), which are not release source.
	FMT_OUT="$(git ls-files '*.go' | xargs gofmt -l)"
	if [ -n "$FMT_OUT" ]; then
		fail "gate:gofmt" "gofmt -l reports unformatted tracked files:"$'\n'"$FMT_OUT"
	fi
	ok "gate:gofmt" "gofmt -l clean (tracked .go files)"

	printf -- '- generated-code freshness (FlatBuffers)...\n'
	if command -v flatc >/dev/null 2>&1; then
		TMPDIR_FB="$(mktemp -d)"
		trap 'rm -rf "$TMPDIR_FB"' EXIT
		# Regenerate into a temp tree and diff against the committed output, so the
		# working tree is never mutated by the check.
		mkdir -p "$TMPDIR_FB/fb"
		flatc --go -o "$TMPDIR_FB/fb" pkg/workflow/schema/workflow_data.fbs || fail "gate:generated" "flatc regeneration failed"
		if ! diff -ru internal/workflow/fb "$TMPDIR_FB/fb" >/dev/null; then
			fail "gate:generated" "committed FlatBuffers code differs from a fresh flatc regeneration (run: make generate-fb)"
		fi
		ok "gate:generated" "FlatBuffers generated code is fresh"
	else
		printf 'PREFLIGHT WARN [gate:generated]: flatc not installed — skipping freshness diff (CI runs this).\n' >&2
	fi
fi

printf '\nPREFLIGHT PASSED for %s (candidate %s)\n' "$TAG" "$CANDIDATE_SHA"
