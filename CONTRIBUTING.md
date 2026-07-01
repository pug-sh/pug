# Contributing to Pug

Thanks for your interest in Pug! 🐶 Pug is an open-source product analytics
platform, and contributions of every kind are welcome — bug reports, docs,
features, and questions.

- 💬 **Chat & get help:** [Pug Discord](https://discord.gg/kDNHDWcBHP)
- 🐛 **Bugs & feature requests:** [GitHub Issues](https://github.com/pug-sh/pug/issues)
- 📜 **Be kind:** all participation is governed by our [Code of Conduct](CODE_OF_CONDUCT.md).

## Ways to contribute

- **Report a bug** — open an issue with steps to reproduce, your OS, the Pug
  version/commit, and relevant logs.
- **Ask a question** — the fastest place is `#help` on [Discord](https://discord.gg/kDNHDWcBHP).
  Please don't open issues for usage questions.
- **Propose a change** — for anything non-trivial, open an issue (or start a
  thread in `#dev` on Discord) to discuss the approach before writing code.
- **Pick up a good first issue** — look for the
  [`good first issue`](https://github.com/pug-sh/pug/labels/good%20first%20issue) label.

## Development setup

**Prerequisites:** [Go 1.26+](https://go.dev/dl/) and Docker (with Docker
Compose) for the local infrastructure.

```bash
# Install Go tool dependencies (golangci-lint, sqlc, templ, buf via `go tool`)
make install-go-deps

# Build the binary -> bin/pug
make build

# Start dev infrastructure (PostgreSQL, ClickHouse, NATS)
make infra

# Run migrations
./bin/pug postgres migrate
./bin/pug nats migrate
./bin/pug clickhouse migrate

# Start the dev server + workers together
./bin/pug dev
```

Environment variables are documented in [`.env.example`](.env.example). Deeper
architecture notes live in [`docs/architecture/`](docs/architecture/), and
conventions the codebase follows are in [`CLAUDE.md`](CLAUDE.md) — please skim it
before your first PR.

## Before you open a pull request

Run these locally — CI (`ci.yaml`, `buf-ci.yaml`) enforces the same:

```bash
make fmt     # goimports on changed Go files
make lint    # golangci-lint
make test    # go test ./... -race
```

If you changed generated inputs, regenerate and commit the output:

```bash
make sqlc    # after editing SQL under schema/postgres/queries/
make rpc     # after editing .proto files
make templ   # after editing .templ email templates
```

Do **not** hand-edit anything under `internal/gen/` — it is generated.

## Pull request guidelines

- **Branch** off `main` and keep PRs focused on one change.
- **Commit messages** follow [Conventional Commits](https://www.conventionalcommits.org/)
  with a scope, matching the existing history — e.g. `fix(auth): ...`,
  `feat(insights): ...`, `test(dashboards): ...`, `docs(readme): ...`.
- **Link the issue** your PR addresses and describe the change and how you tested it.
- **Keep CI green** and add tests for new behavior (the suite runs with the race
  detector).

## License

By contributing, you agree that your contributions will be licensed under the
[GNU AGPL v3.0](LICENSE), the same license that covers the project.
