//go:build !go1.25

package workflow

// detTaxToolchainCalibrated is false before Go 1.25 — the det-tax absolute alloc
// budget is calibrated for Go 1.25+ (arm64 283/277); older toolchains have a different
// alloc baseline, so det-tax skips. See go125_on_test.go for the rationale.
const detTaxToolchainCalibrated = false
