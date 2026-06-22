# Release Process

This document outlines the process for creating and publishing releases of Flow Orchestrator.

## Release Types

Flow Orchestrator follows [Semantic Versioning](https://semver.org/):

- **Major releases** (X.0.0): Incompatible API changes
- **Minor releases** (0.X.0): New functionality in a backward-compatible manner
- **Patch releases** (0.0.X): Backward-compatible bug fixes
- **Pre-releases**: Alpha, beta, and release candidate versions

## Release Checklist

### 1. Preparation

- [ ] Review and resolve all issues targeted for the release
- [ ] Update the version in `pkg/workflow/version.go`
- [ ] Update CHANGELOG.md with all notable changes
- [ ] Run the full test suite and ensure all tests pass
- [ ] Update relevant documentation to reflect any changes
- [ ] Ensure examples are working with the new version

### 2. Prepare the release on `main`

The default branch is **`main`**; there is no `develop` branch (see
[CONTRIBUTING.md](../../CONTRIBUTING.md)). Releases are cut by tagging a commit on
`main`. Prepare the release as a normal topic-branch PR into `main`:

```bash
# Branch off main, update the version + CHANGELOG, open a PR
git checkout main
git pull
git checkout -b release-prep-v1.2.0
# ...bump pkg/workflow.Version, finalize CHANGELOG.md...
```

### 3. Final Checks

- [ ] Run the full test suite (`go test ./... -race`)
- [ ] Verify that all documentation is up-to-date
- [ ] Ensure all examples build and run
- [ ] Verify that the CHANGELOG.md is complete and accurate

### 4. Tag on `main` (triggers the release workflow)

Once the release-prep PR is merged, tag the merge commit on `main`. **The tag push
triggers `.github/workflows/release.yml`** (the workflow is `on: push: tags`),
which builds the release and generates SLSA provenance:

```bash
git checkout main
git pull
git tag -a v1.2.0 -m "v1.2.0"
git push origin v1.2.0
```

### 5. GitHub Release

The release workflow handles building and attaching provenance on the tag push.
Confirm the resulting release on the
[GitHub Releases page](https://github.com/ppcavalcante/flow-orchestrator/releases),
add/curate release notes from CHANGELOG.md, and mark it as a pre-release if the
version carries an `-alpha`/`-beta`/`-rc` suffix.

### 6. Verify the published module

```bash
GOPROXY=proxy.golang.org go list -m github.com/ppcavalcante/flow-orchestrator@v1.2.0
```

## Handling Pre-releases

For pre-releases (alpha, beta, rc), follow the same process but use an appropriate
version suffix on the tag:

```bash
# For a beta release
git tag -a v1.2.0-beta.1 -m "v1.2.0-beta.1"
git push origin v1.2.0-beta.1
```