# Repository Guidelines

## Project Structure & Module Organization
The Go entrypoint (`main.go`) wires HTTP/RPC services by composing packages under `internal/`, which owns both business logic and generated SQL clients in `internal/repo/gen`. Shared helpers live in `pkg/`, while `proto/` and the Buf configs (`buf.yaml`, `buf.gen.yaml`) describe RPC contracts that feed SDK generation. Database schemas and sqlc inputs are stored in `schema/` plus `sqlc.yaml`. Front-end assets sit in `web/`, and `infra/dev/` contains Docker Compose recipes for Postgres and companion services used in development.

## Build, Test, and Development Commands
- `make build` – compile the Go binary into `bin/cotton`.
- `make test` – run `go test ./...` across all packages; prefer this before any push.
- `make lint` – execute `buf lint` to validate protobuf definitions.
- `make rpc` / `make gen-ts` – lint and regenerate Go/TS RPC stubs from `proto/`.
- `make sqlc` – refresh database accessors in `internal/repo/gen`; rerun after editing `schema/`.
- `make infra-up` / `make infra-down` – start or stop the Docker services defined under `infra/dev/`.
- `make psql` – open a psql shell to the dev Postgres when Docker is running.

## Coding Style & Naming Conventions
Use Go 1.25.3 and keep files formatted via `gofmt` (or `go fmt ./...`). Packages follow standard Go directory casing; exported identifiers use PascalCase, locals camelCase, and database columns snake_case to match schema files. Proto files must pass `buf lint`; keep RPC service names singular (`NotificationService`) and messages suffixed with `Request`/`Response`.

## Testing Guidelines
Unit and integration tests live beside the code under test as `*_test.go`. Favor table-driven tests and descriptive `TestFeatureScenario` names. Run `make test` (or `go test ./internal/...`) before committing. When touching SQL, add regression cases using fixtures that mirror `schema/` defaults and keep coverage steady—flag drops in the PR.

## Commit & Pull Request Guidelines
The Git history uses terse, present-tense summaries (`adds notification sending`). Mirror that style: imperative verb, lower-case, and under 70 characters. Each commit should be logically scoped (e.g., “adds delivery service handler”) and include regenerated code when interfaces change. Pull requests need a short problem statement, a bullet list of major changes, test evidence (`make test` output), and links to tracking issues. Include screenshots or API samples whenever UI or contract behavior changes and note any manual infra steps required for review.
