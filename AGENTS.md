# Repository Guidelines

## Scope & Intent
- This file guides agentic coding tools working in this repo.
- Follow these rules before reading or writing code.
- Prefer minimal, targeted changes that match existing patterns.
- Regenerate generated code when you change its inputs.
- Ask questions when requirements are ambiguous.

## Project Structure & Modules
- `main.go` wires HTTP/RPC services and command setup.
- Business logic lives under `internal/` (core, rpc, workers, commands).
- Database accessors are generated into `internal/gen/repo` via sqlc.
- Protobuf-generated Go lives under `internal/gen/proto`.
- Shared libraries live in `pkg/` (logging, postgres, nats, clickhouse).
- Database schema files live under `schema/`; sqlc config is `sqlc.yaml`.
- Buf configs are `buf.yaml` and `buf.gen.yaml` under repo root.
- Infrastructure helpers live in `infra/dev/` with Docker Compose.
- Keep generated code and hand-written code separated.

## Build, Lint, Test (Backend)
- `make build` builds the Go binary into `bin/cotton`.
- `make test` runs `go test ./...` across all packages.
- `make lint` runs `buf lint` for protobuf definitions.
- `make rpc` runs `buf generate` (after `lint`).
- `make sqlc` regenerates SQL clients in `internal/gen/repo`.
- `make install-go-deps` installs sqlc and protoc plugins.
- `make install-all-deps` prints SDK generation dependencies.
- Run `go fmt ./...` after broad refactors.
- Use `go test ./...` before pushing changes.

## Local Dev Utilities
- `make infra-up` starts dev services via Docker Compose.
- `make infra-up-fg` starts dev services in foreground.
- `make infra-down` stops dev services.
- `make infra` is an alias for `make infra-up`.
- `make psql` opens a psql shell to dev Postgres.

## Run a Single Test (Go)
- `go test ./internal/... -run TestName` runs a test by name.
- `go test ./internal/core/... -run TestName -count=1` avoids cached results.
- `go test ./internal/core/auth -run TestAuthFlow` runs a package’s test.
- `go test ./internal/... -run TestName/Subtest` runs a subtest.
- `go test ./internal/... -run TestName -v` for verbose output.
- Use `-count=1` when validating flakey behavior.
- Prefer package-scoped tests over `./...` when iterating.

## Formatting & Tooling
- Use Go 1.25.3 (from `go.mod`).
- Always format Go code with `gofmt` or `go fmt ./...`.
- Keep protobufs passing `buf lint` before regenerating.
- Do not edit files under `internal/gen/` manually.
- Regenerate code after updates to `proto/` or `schema/`.
- Keep `go.mod` and `go.sum` in sync when adding deps.

## Go Imports
- Group imports: standard library, blank line, third-party, blank line, internal.
- Avoid unused imports; keep alphabetical order within each group.
- Prefer explicit imports rather than dot imports.
- Avoid aliasing unless needed to resolve conflicts or clarify usage.
- Keep import grouping consistent within a file.

## Types & APIs
- Put `context.Context` as the first argument to public functions.
- Return early on errors; keep the happy path linear.
- Prefer explicit types over `any` for public APIs.
- Use `time.Duration` and `time.Time` for time values.
- Use pointer receivers when mutating struct state.
- Prefer constructor functions like `NewService` for setup.

## Naming Conventions
- Packages are lowercase, short, and avoid stutter.
- Exported identifiers use PascalCase; locals use camelCase.
- Constants use camelCase or ALL_CAPS only when conventional.
- SQL columns use snake_case and match `schema/` definitions.
- RPC services are singular (e.g., `NotificationService`).
- Protobuf messages end with `Request`/`Response` when applicable.

## Error Handling
- Return errors rather than panicking for expected failures.
- Wrap errors with context using `fmt.Errorf("...: %w", err)`.
- Use `errors.Is`/`errors.As` for typed comparisons.
- In RPC handlers, wrap with `connect.NewError` and set codes.
- Avoid leaking sensitive data in RPC error messages.
- Log before returning internal errors where helpful.

## Logging
- Use structured logging via `log/slog`.
- Prefer `slog.ErrorContext` with `slog.Any("error", err)`.
- Use `pkg/logger/slogx.Error(err)` for error attributes where used.
- Include key identifiers in log fields (project IDs, campaign IDs).
- Avoid logging secrets, tokens, or credentials.
- Avoid `log.Fatal`; use `os.Exit(1)` only during startup failures.

## RPC Patterns (Connect)
- RPC handlers live under `internal/rpc/...`.
- Use `connect.NewResponse` to build responses.
- Convert domain models to protobuf using helper mappers.
- Use `connect.NewError` for non-OK responses.
- Propagate auth errors with `connect.CodeUnauthenticated`.
- Use `connect.CodeNotFound` when ownership checks fail.

## Database & SQLC
- SQL schemas are in `schema/` and drive sqlc generation.
- `make sqlc` regenerates `internal/gen/repo` (do this after schema edits).
- Use `dbread` for read paths and `dbwrite` for write paths.
- Keep SQL parameter names in snake_case to match schema.
- Prefer passing explicit params structs for queries.
- Keep read and write pools (`pgRO`, `pgW`) distinct.

## Protobuf & Buf
- Proto sources live under `proto/`.
- `make lint` runs `buf lint` and must pass before `make rpc`.
- `make rpc` regenerates Go RPC stubs under `internal/gen/proto`.
- Do not hand-edit generated protobuf or connect files.
- Keep proto changes backwards compatible when possible.
- Use consistent field naming and avoid breaking field numbers.

## Tests
- Tests live beside code as `*_test.go`.
- Prefer table-driven tests (`[]struct{...}`) with descriptive cases.
- Name tests `TestFeatureScenario` and subtests `t.Run("case")`.
- Add regression tests when fixing bugs.
- When touching SQL, add tests/fixtures reflecting schema defaults.
- Keep tests deterministic; avoid time-based flakiness.
- Use helpers to reduce repeated setup in tests.

## Commit & PR Expectations
- Commit messages are imperative, present tense, lowercase.
- Keep summaries under 70 characters.
- Regenerated code should be included in the same commit.
- PRs should include a short problem statement and test evidence.
- Mention any manual infra steps needed for reviewers.

## Tooling Notes
- No Cursor rules found in `.cursor/rules/` or `.cursorrules`.
- No Copilot rules found in `.github/copilot-instructions.md`.
- Follow any future tool-specific rules if they appear.

## Safety & Hygiene
- Avoid editing generated code unless asked explicitly.
- Keep changes scoped to the request.
- Avoid introducing new dependencies without a clear need.
- Update or add docs only when the change requires it.
- Avoid committing secrets or credentials.
