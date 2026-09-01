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

## License and the CLA

Pug is distributed under the [GNU AGPL v3.0](LICENSE).

Before your first pull request is merged, you'll be asked to sign the
[Contributor License Agreement](CLA.md). Comment `/sign` on your pull request and
that is done — it records your signature on the base branch as a commit authored
by you. To sign by hand instead, add yourself to
[`tools/cla/signatures.json`](tools/cla/signatures.json) in the same pull request; the CLA
check prints the entry for you, with your GitHub id already filled in. You do it
once per CLA version; it covers everything you contribute afterwards.

A `Co-authored-by:` trailer naming a person means they have to sign too — they
comment `/sign` on the same pull request. Use a GitHub noreply address — `<login>@users.noreply.github.com`
or the `<id>+<login>` form GitHub itself writes. That is the only shape the check
can resolve to an account, and a trailer it cannot identify blocks the pull request
whether or not that person has signed. One naming an AI assistant is ignored — it
holds no copyright — so there is no need to rewrite commits to strip it.

The file is append-only, and CI enforces it: an existing entry can't be edited or
removed, the only entry you can add is your own — in a pull request you opened, so
a co-author signs in one of their own — and `cla_version` is set on the base branch
rather than by a contribution, so a new signature is always recorded against the
version in force.

**You keep your copyright.** The CLA is a license, not an assignment — you stay
free to use and relicense your own work however you like. What it adds on top of
the AGPL is the right for us to also offer Pug under separate commercial terms to
organizations that can't adopt AGPL software. That funds the project's
development. Section 2.3 is the limit on it: whatever else we license your
contribution under, we must also keep licensing it under the license Pug was
using when you submitted it — the AGPL. Keeping Pug as a whole under the AGPL is
our intent, and section 2.3 is what guarantees it for your own work.
