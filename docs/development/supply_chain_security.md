# Supply Chain Security

This document outlines our approach to supply chain security for Flow Orchestrator, including our current practices and future goals.

## Current Security Practices

Flow Orchestrator implements several security measures to protect the supply chain:

| Practice | Status | Implementation |
|----------|--------|----------------|
| **Dependency Management** | ✅ Implemented | Automated updates via Dependabot |
| **Vulnerability Scanning** | ✅ Implemented | GitHub's dependency scanning |
| **SBOM Generation** | ✅ Implemented | Using Anchore's sbom-action in our release workflow |
| **Code Reviews** | ✅ Implemented | All changes are reviewed before merging |
| **Automated Testing** | ✅ Implemented | Comprehensive test suite with high coverage requirements |
| **Security Linting** | ✅ Implemented | Using gosec and other security-focused linters |

## SLSA Framework

[SLSA (Supply chain Levels for Software Artifacts)](https://slsa.dev/) is a security framework that provides a checklist of standards and controls to prevent tampering, improve integrity, and secure packages and infrastructure.

As a smaller project, we're taking an incremental approach to SLSA compliance, focusing on the most impactful security practices first.

**Already in place** (`.github/workflows/release.yml`):

- **Build provenance**: the release workflow runs the SLSA GitHub generator on a
  tag push, producing signed provenance attestations attached to the release
  (keyless OIDC signing; the generator is referenced by tag, as required for its
  provenance-identity verification).
- **Dependency pinning**: third-party GitHub Actions are SHA-pinned and
  dependencies are tracked via Dependabot; `govulncheck` runs (blocking) in CI.

## Future Security Enhancements

We plan to enhance our supply chain security further:

1. **Hermetic Builds**: Ensure builds are self-contained and reproducible
2. **Enhanced CI/CD Security**: Additional security checks in the pipeline

## References

- [SLSA Framework](https://slsa.dev/)
- [GitHub's Supply Chain Security Features](https://docs.github.com/en/code-security/supply-chain-security)
- [SBOM (Software Bill of Materials)](https://www.cisa.gov/sbom) 