//go:build go1.25

package workflow

// detTaxToolchainCalibrated is true on Go 1.25+ — the toolchain the det-tax absolute
// alloc budget (283/277, arm64 baseline) is calibrated for. The absolute alloc COUNT
// drifts across Go MINOR versions (Go 1.24 amd64 reads +5: 288/282) — a per-version
// artifact, NOT our determinism tax. A real regression from OUR code adds allocs on
// EVERY toolchain, so enforcing on the reference toolchain (1.25, == dev arm64 1.25.1)
// still catches it; det-tax SKIPS on 1.24 rather than mis-flagging the toolchain delta.
// Build-tagged pair: go125_on_test.go (go1.25) / go125_off_test.go (!go1.25).
const detTaxToolchainCalibrated = true
