# Security Policy

## Supported Versions

Flow Orchestrator is **pre-1.0 alpha**: every published tag is a pre-release, and there is no
long-term-support line. **Only the latest published pre-release receives security updates.** Older
alphas are not backported — upgrade to the latest tag.

| Version | Supported |
| ------- | --------- |
| `v0.21.0-alpha` (latest published tag) | :white_check_mark: |
| all earlier `0.x` alphas | :x: |

**Note**: this table names the latest tag at the time of writing. The authoritative list of published
releases is [the tag list](https://github.com/ppcavalcante/flow-orchestrator/tags) and
[CHANGELOG.md](CHANGELOG.md); if this table names a tag older than the highest published one, the
policy above ("only the latest") governs and this table is stale.

## Reporting a Vulnerability

We take the security of Flow Orchestrator seriously. If you believe you've found a security vulnerability, please follow these steps:

1. **Do not disclose the vulnerability publicly**
2. Report the vulnerability through [GitHub Security Advisories](https://github.com/ppcavalcante/flow-orchestrator/security/advisories/new)
3. Provide detailed information including:
   - Description of the vulnerability
   - Steps to reproduce
   - Potential impact
   - Suggested fixes (if any)

## Response Process

- We will acknowledge receipt of your report within 3 business days
- We will provide an assessment and expected timeline for a fix within 10 business days
- We will keep you informed of our progress
- Once fixed, we will publicly acknowledge your responsible disclosure (unless you prefer anonymity)

## Security Measures

Flow Orchestrator implements several security measures:

- Comprehensive test suite with high coverage requirements
- Automated dependency updates via Dependabot
- Software Bill of Materials (SBOM) generation for releases
- Linting via golangci-lint v2 (pinned), currently reporting zero findings on the configured linter set
- Code reviews for all changes

For the persistence layer's trust model and known limitations (caller-controlled
input, traversal and corruption guards, and the explicit not-adversarial-proof
boundary), see [docs/guides/persistence.md](docs/guides/persistence.md) (Trust & Safety).

## Security Roadmap

We are committed to improving our security posture and are working toward:

- Enhancing our CI/CD pipeline with additional security checks
- Implementing automated dependency vulnerability scanning
- Adding runtime security features
- Exploring supply chain security best practices

## Security Best Practices for Users

When using Flow Orchestrator, consider these security best practices:

1. Keep your dependencies up to date
2. Use the latest stable version of Flow Orchestrator
3. Implement proper access controls for any workflow data
4. Validate all inputs to your workflows
5. Consider using the validation middleware for additional security 
