<!-- PR title must follow Conventional Commits, e.g. `fix(store): reject oversize .fb before decode` -->

## What & why

<!-- What does this change and why? Link any related issue (#123). -->

## Checklist

- [ ] Tests added/updated and `go test ./... -race` passes locally
- [ ] `make lint` is clean (golangci-lint v2.12.2, run via `go run` — no local install needed)
- [ ] `gofmt`/`go vet` clean
- [ ] Documentation updated for any public-API or behavior change
- [ ] `CHANGELOG.md` entry added under `[Unreleased]`
- [ ] PR title follows [Conventional Commits](https://www.conventionalcommits.org/)
- [ ] For any change to a covered public surface: impact noted here and in `STABILITY.md`

## Notes for reviewers

<!-- Anything reviewers should focus on: tricky areas, intentional breaks, follow-ups. -->
