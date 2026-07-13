<!--
Thanks for contributing to pulsys!

PR TITLE must follow Conventional Commits, e.g.:
  feat(cache): add per-repo disk quota enforcement
  fix(parser): reject duplicate Content-Length headers
-->

## What does this PR do?

<!-- A clear, concise description of the change and the motivation. -->

## Related issues

<!-- e.g. Closes #123 -->

## Type of change

- [ ] Bug fix (non-breaking change that fixes an issue)
- [ ] New feature (non-breaking change that adds functionality)
- [ ] Breaking change (fix or feature that changes existing behavior)
- [ ] Documentation only
- [ ] Build / CI / tooling

## Checklist

- [ ] My commits are signed off (`git commit -s`) per the [DCO](https://developercertificate.org/).
- [ ] The PR title follows [Conventional Commits](https://www.conventionalcommits.org/).
- [ ] `go test -race ./...` passes locally.
- [ ] `gofmt -s` and `go vet ./...` are clean.
- [ ] I added/updated tests for my change (regression test for bug fixes).
- [ ] I updated docs (README / DEVELOPMENT / docs / chart values) where relevant.

## Performance impact (if touching the hot path)

<!--
If this change touches the warm path (coreserver / cache / proxy), include a
benchstat before/after table. The warm path is 0 alloc / 1 sendfile and we keep
it that way.
-->

## Security considerations

<!--
Any impact on the parser, auth, or deployment surface? If this is a security
FIX, please coordinate via SECURITY.md rather than describing the exploit here.
-->
