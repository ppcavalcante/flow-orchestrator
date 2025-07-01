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

### 2. Release Branch Creation

For minor and major releases:

```bash
# For a new minor/major release (e.g., v1.2.0)
git checkout develop
git pull
git checkout -b release/v1.2.0
```

For patch releases:

```bash
# For a patch release (e.g., v1.1.1)
git checkout main
git pull
git checkout -b hotfix/v1.1.1
```

### 3. Final Checks

- [ ] Run the full test suite on the release branch
- [ ] Verify that all documentation is up-to-date
- [ ] Ensure all examples work correctly
- [ ] Verify that the CHANGELOG.md is complete and accurate

### 4. Merge to Main

Once all checks pass, merge the release branch into main:

```bash
git checkout main
git pull
git merge --no-ff release/v1.2.0 -m "chore: release v1.2.0"
git tag -a v1.2.0 -m "v1.2.0"
git push origin main --tags
```

### 5. Merge Back to Develop

For minor and major releases, merge the changes back to develop:

```bash
git checkout develop
git pull
git merge --no-ff main -m "chore: merge release v1.2.0 back to develop"
git push origin develop
```

### 6. Create GitHub Release

1. Go to the [GitHub Releases page](https://github.com/ppcavalcante/flow-orchestrator/releases)
2. Click on "Draft a new release"
3. Select the tag version
4. Set the title to the version number (e.g., "v1.2.0")
5. Copy the release notes from CHANGELOG.md
6. Check "This is a pre-release" if applicable
7. Click "Publish release"

### 7. Publish the Package

```bash
GOPROXY=proxy.golang.org go list -m github.com/ppcavalcante/flow-orchestrator@v1.2.0
```

## Handling Pre-releases

For pre-releases (alpha, beta, rc), follow the same process but use appropriate version suffixes:

```bash
# For a beta release
git tag -a v1.2.0-beta.1 -m "v1.2.0-beta.1"
```

## Detailed Git Workflow

For a more detailed explanation of the git workflow and branching strategy, see the [Git Workflow](../gw.md) documentation. 