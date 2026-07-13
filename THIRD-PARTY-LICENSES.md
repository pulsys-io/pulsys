# Third-Party Licenses

pulsys is licensed under the [Apache License 2.0](LICENSE). It builds on the
third-party components listed below. This file is a convenience summary; each
dependency's authoritative license travels with its own source distribution
(for Go modules, in the module cache; for vendored code, in-tree).

To regenerate a full, machine-readable inventory:

```bash
go install github.com/google/go-licenses@latest
go-licenses report ./... > third-party-report.txt
# or produce the SBOM the release pipeline attaches:
go install github.com/anchore/syft/cmd/syft@latest
syft . -o spdx-json
```

## Vendored in-tree

| Component | Version | License | Path |
| --- | --- | --- | --- |
| [llhttp](https://github.com/nodejs/llhttp) | submodule (v9.4.2) | MIT | `internal/coreserver/vendor/llhttp/` |

The llhttp parser is vendored as a git submodule and retains its upstream
`LICENSE` (MIT, Copyright (c) 2018 Fedor Indutny and contributors). It is a
test-fixture corpus only; the corpus tests skip when the submodule is not
initialized.

## Go module dependencies (direct)

These are pulled at build time via Go modules and are not redistributed in
source form in this repository. License identifiers are the SPDX expressions
published by each project.

| Module | License |
| --- | --- |
| github.com/coreos/go-oidc/v3 | Apache-2.0 |
| github.com/golang-migrate/migrate/v4 | MIT |
| github.com/jackc/pgx/v5 | MIT |
| github.com/prometheus/client_golang | Apache-2.0 |
| github.com/riverqueue/river (+ riverdriver, rivertype) | MPL-2.0 |
| github.com/testcontainers/testcontainers-go (+ modules/postgres) | MIT |
| golang.org/x/net | BSD-3-Clause |
| golang.org/x/oauth2 | BSD-3-Clause |
| golang.org/x/sys | BSD-3-Clause |

Indirect dependencies (transitively required) are predominantly licensed under
the permissive Apache-2.0, MIT, and BSD families. Run the `go-licenses` or
`syft` commands above for the complete, current inventory, including indirect
modules and exact versions resolved by `go.sum`.

> Note: `github.com/riverqueue/river` is distributed under the Mozilla Public
> License 2.0 (file-level copyleft). pulsys uses it as an unmodified library
> dependency; MPL-2.0 obligations attach only to modifications of River's own
> files, which this project does not make.
