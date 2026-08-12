#!/usr/bin/env bash
#
# run_tlc.sh — model-check the TLA+ capstones with TLC (AUD-046 / V-04).
#
# The formal specs in specs/ were only ever run BY HAND, so they could rot
# relative to the code and tools with nothing to catch it. This runs the
# load-bearing capstones — the level-executor safety+liveness proofs and the
# durable crash-resume algorithm — from CI, against a PINNED tools jar verified
# by SHA-256 (a floating `releases/latest` jar makes a green non-reproducible).
#
# Scope (deliberate, documented): this runs the "No error has been found"
# capstones that back the durability claims — NOT the `*Break` mutation configs,
# which INVERT the pass condition (TLC is EXPECTED to report a violation there).
# Grading those correctly needs per-config expected-outcome metadata that lives
# in specs/README.md prose, not in the files; wiring that up is a documented
# follow-on. Adding a capstone here is one line in CAPSTONES below.
#
# Env:
#   JAVA               java binary (default: `java` on PATH; must be 17+)
#   TLA_TOOLS_JAR      path to a cached tla2tools.jar (skips download if valid)
#   TLA_TOOLS_CACHE    download dir when TLA_TOOLS_JAR is unset (default: .tlacache)
#
# Usage: scripts/formal/run_tlc.sh
set -euo pipefail

# --- pinned tools jar -------------------------------------------------------
# Pin BOTH the version and its content hash. The version selects the release;
# the SHA-256 is the reproducibility gate — a jar that does not match is refused
# rather than silently model-checked with an unknown tool.
TLA_TOOLS_VERSION="v1.8.0"
TLA_TOOLS_SHA256="ab323b79802aedc3203b3f9af37c6aca3ed43f4e0225b36f2aa77b26de46c05f"
TLA_TOOLS_URL="https://github.com/tlaplus/tlaplus/releases/download/${TLA_TOOLS_VERSION}/tla2tools.jar"

# --- verified capstone table (cfg -> model module) --------------------------
# Every pair below was run locally on TLC ${TLA_TOOLS_VERSION} and reported
# "No error has been found"; the trailing number is the expected distinct-state
# count (state-space size, published for drift visibility — a large swing means
# the model or a constant changed). The model is the TLC MODULE NAME (no .tla).
CAPSTONES=(
  "Executor.cfg               MCExecutor              13"
  "ExecutorHardFail.cfg       MCExecutor              14"
  "ExecutorConc1.cfg          MCExecutor              12"
  "DurableExecutorClean.cfg   MCDurableExecutor       426"
  "DurableExecutor.cfg        MCDurableExecutor       426"
  "DurableExecutorHardFail.cfg MCDurableExecutor      318"
  "M10DurableExecutor.cfg     MCM10DurableExecutor    14380"
  "M10FailResume.cfg          MCM10FailResume         915"
)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
SPECS_DIR="${REPO_ROOT}/specs"
JAVA="${JAVA:-java}"

log() { printf '%s\n' "$*" >&2; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# resolve_jar echoes a path to a tla2tools.jar whose SHA-256 matches the pin,
# downloading it once into the cache if necessary. Refuses a mismatch.
resolve_jar() {
  local jar="${TLA_TOOLS_JAR:-}"
  if [[ -n "$jar" && -f "$jar" ]]; then
    local got; got="$(sha256_of "$jar")"
    if [[ "$got" != "$TLA_TOOLS_SHA256" ]]; then
      log "FATAL: TLA_TOOLS_JAR $jar sha256 $got != pinned $TLA_TOOLS_SHA256"; exit 3
    fi
    printf '%s' "$jar"; return 0
  fi
  local cache="${TLA_TOOLS_CACHE:-${REPO_ROOT}/.tlacache}"
  mkdir -p "$cache"
  jar="${cache}/tla2tools-${TLA_TOOLS_VERSION}.jar"
  if [[ ! -f "$jar" || "$(sha256_of "$jar")" != "$TLA_TOOLS_SHA256" ]]; then
    log "Fetching TLA+ tools ${TLA_TOOLS_VERSION} ..."
    curl -fsSL -o "$jar" "$TLA_TOOLS_URL"
    local got; got="$(sha256_of "$jar")"
    if [[ "$got" != "$TLA_TOOLS_SHA256" ]]; then
      log "FATAL: downloaded jar sha256 $got != pinned $TLA_TOOLS_SHA256"; rm -f "$jar"; exit 3
    fi
  fi
  printf '%s' "$jar"
}

main() {
  if ! "$JAVA" -version >/dev/null 2>&1; then
    log "FATAL: no working java (\$JAVA=$JAVA). TLC needs a 17+ JRE."; exit 2
  fi
  local jar; jar="$(resolve_jar)"
  log "TLC jar: $jar (${TLA_TOOLS_VERSION})"
  log "java:    $("$JAVA" -version 2>&1 | head -1)"

  local summary="${TLC_SUMMARY:-${REPO_ROOT}/tlc-summary.txt}"
  : > "$summary"
  local failures=0 n=0

  cd "$SPECS_DIR"
  for row in "${CAPSTONES[@]}"; do
    # shellcheck disable=SC2086
    set -- $row; local cfg="$1" model="$2" want="${3:-}"
    n=$((n + 1))
    # A UNIQUE metadir per run is load-bearing: back-to-back TLC runs against the
    # same module in one dir collide on the default states/ dir and throw
    # StringIndexOutOfBoundsException (an empty metadir path). Isolate every run.
    local md; md="$(mktemp -d)"
    local out rc=0
    out="$("$JAVA" -cp "$jar" tlc2.TLC -metadir "$md" -config "$cfg" "$model" 2>&1)" || rc=$?
    rm -rf "$md"

    # The FINAL distinct-state count is the LAST unformatted summary line
    # ("<N> states generated, <M> distinct states found, ..."). Progress lines
    # carry thousands separators (14,380) and intermediate counts, so a naive
    # grep|head grabs a comma-mangled snapshot, not the deterministic final.
    local states; states="$(printf '%s' "$out" \
      | grep -E '^[0-9]+ states generated, [0-9]+ distinct states found' \
      | tail -1 | sed -E 's/^[0-9]+ states generated, ([0-9]+) distinct states found.*/\1/')"
    if printf '%s' "$out" | grep -q "No error has been found"; then
      local note=""
      if [[ -n "$want" && -n "$states" && "$states" != "$want" ]]; then
        note=" (state-count drift: got ${states}, table says ${want})"
      fi
      printf 'PASS  %-28s %-22s states=%s%s\n' "$cfg" "$model" "${states:-?}" "$note" | tee -a "$summary" >&2
    else
      failures=$((failures + 1))
      printf 'FAIL  %-28s %-22s rc=%s\n' "$cfg" "$model" "$rc" | tee -a "$summary" >&2
      printf '%s\n' "$out" | grep -iE 'error|violat|exception' | head -5 >&2 || true
    fi
  done

  log ""
  log "TLC capstones: $((n - failures))/${n} passed  (summary: $summary)"
  [[ "$failures" -eq 0 ]]
}

main "$@"
