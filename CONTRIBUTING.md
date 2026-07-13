# Contributing to pulsys

Thanks for your interest in improving pulsys! This document covers how to get
your change merged smoothly. By participating you agree to abide by our
[Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to contribute

- **Report bugs** and **request features** via [issues](https://github.com/pulsys-io/pulsys/issues)
  (use the templates).
- **Ask questions** / discuss ideas in
  [Discussions](https://github.com/pulsys-io/pulsys/discussions).
- **Send pull requests** for fixes, features, docs, and tests.
- **Report security vulnerabilities** privately — see [SECURITY.md](SECURITY.md).
  Please do **not** open a public issue for a vulnerability.

Looking for a place to start? Issues labeled
[`good first issue`](https://github.com/pulsys-io/pulsys/labels/good%20first%20issue)
and [`help wanted`](https://github.com/pulsys-io/pulsys/labels/help%20wanted) are
a good entry point.

## Development setup

See [DEVELOPMENT.md](DEVELOPMENT.md) for prerequisites, the repository map, the
local stack, and how to run the tests and benchmarks.

Clone the repository (including the llhttp parser submodule):

```bash
git clone --recurse-submodules https://github.com/pulsys-io/pulsys.git
cd pulsys
```

The fast inner loop:

```bash
go build ./...
go test -race ./...
gofmt -s -w .
go vet ./...
```

## Developer Certificate of Origin (DCO)

We require a [Developer Certificate of Origin](https://developercertificate.org/)
sign-off on every commit. This is a lightweight statement that you wrote the
patch (or have the right to submit it). Add it by committing with `-s`:

```bash
git commit -s -m "fix: correct range handling on partial cache hit"
```

This appends a trailer to your commit message:

```
Signed-off-by: Your Name <you@example.com>
```

The name/email must match your Git identity. A DCO check runs on every PR.

## Pull requests

1. **Fork** and create a topic branch from `main`.
2. Keep PRs focused; smaller PRs are reviewed faster.
3. Add or update tests. New behavior needs a test; bug fixes need a regression
   test. Security-relevant changes belong in `internal/security/*` — see
   [DEVELOPMENT.md](DEVELOPMENT.md#testing).
4. If your change affects the warm path, include a `benchstat` before/after
   table (the warm path is **0 alloc / 1 sendfile** and we keep it that way).
5. Update docs (`README.md`, `DEVELOPMENT.md`, `docs/`, chart `values.yaml`) when
   behavior or configuration changes.
6. Make sure the full local gate passes: `go test -race ./...`, `gofmt -s`,
   `go vet`, and `govulncheck ./...`.

### PR title: Conventional Commits

PR titles **must** follow [Conventional Commits](https://www.conventionalcommits.org/).
The title becomes the squash-merge commit subject and feeds automated
changelog + release tooling ([release-please](https://github.com/googleapis/release-please)).

```
<type>(optional scope): <description>
```

Common types: `feat`, `fix`, `docs`, `refactor`, `perf`, `test`, `build`, `ci`,
`chore`. Use `feat!:` or a `BREAKING CHANGE:` footer for breaking changes.

Examples:

```
feat(cache): add per-repo disk quota enforcement
fix(parser): reject duplicate Content-Length headers
perf(coreserver): drop one alloc on the 256 KiB warm hit
docs: document the Cognito OIDC setup
```

## Coding standards

We follow the canonical Go references:

- [Effective Go](https://go.dev/doc/effective_go)
- [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)
- [Google Go Style Guide](https://google.github.io/styleguide/go/)
- [Go Doc Comments](https://go.dev/doc/comment) for package and symbol docs.

`gofmt -s` and `go vet` are non-negotiable and enforced in CI.

## Releases & versioning

The project uses [Semantic Versioning](https://semver.org/) and stays on
`v0.x.y` until the API/CLI stabilizes. Releases are automated: merged
Conventional-Commit PRs drive a release-please PR that maintains the
`CHANGELOG.md` and the next version tag; merging it triggers the signed release
build. Maintainers can withdraw a bad tag with a `retract` directive in `go.mod`.

## License

By contributing, you agree that your contributions are licensed under the
project's [Apache License 2.0](LICENSE).
