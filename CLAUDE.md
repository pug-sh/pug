# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Cotton is a push notification platform built with Go, using PostgreSQL, ClickHouse, and NATS for data storage and messaging.

## Build & Run Commands

```bash
# Build the Go binary
make build

# Run tests
make test

# Start dev infrastructure (PostgreSQL, NATS, ClickHouse)
make infra

# Stop infrastructure
make infra-down

# Run database migrations
./bin/cotton postgres migrate
./bin/cotton nats migrate
./bin/cotton clickhouse migrate

# Start development server + workers together
./bin/cotton dev

# Start server only
./bin/cotton server

# Start individual workers
./bin/cotton worker device
./bin/cotton worker campaign
./bin/cotton worker events
./bin/cotton worker profile register
./bin/cotton worker profile identify
./bin/cotton worker profile alias
./bin/cotton worker scheduler
```

### Code Generation

```bash
# Generate sqlc queries (after modifying SQL files)
make sqlc

# Generate protobuf code (after modifying .proto files)
make rpc

# Lint Go code
make lint

# Lint proto files
make lint-proto
```

## Architecture

### Backend (Go)

The backend follows a layered architecture with Connect RPC (HTTP/2):

- **`internal/app/`** - CLI entry points using Cobra, split by feature (server, workers, dev, migrate)
  - `server/rpc/` - RPC handlers that map proto services to business logic
  - `workers/campaigns/`, `workers/devices/`, `workers/profiles/`, `workers/events/`, `workers/scheduler/` - NATS message consumers
- **`internal/core/`** - Business logic layer with service and repo per domain (auth, campaigns, delivery, devices, profiles, projects)
- **`internal/gen/`** - Generated code (do not edit manually)
  - `proto/` - Generated from .proto files via buf
  - `repo/dbread/`, `repo/dbwrite/` - Generated from SQL via sqlc

### Database Pattern

PostgreSQL uses read/write separation:

- Queries in `schema/postgres/queries/read/` generate to `internal/gen/repo/dbread/`
- Queries in `schema/postgres/queries/write/` generate to `internal/gen/repo/dbwrite/`

**sqlc conventions**:

- Query names: PascalCase with uppercase `ID` (e.g., `GetCampaignByID`, `GetProjectsByCustomerID`)
- SQL syntax and identifiers: lowercase (e.g., `select * from campaigns where project_id = @project_id`)
- Partial updates: use `coalesce(nullif(@field, ''), field)` to preserve existing values when empty

### Proto/RPC

Services defined in `proto/` directory. Generated code goes to `internal/gen/proto/`. Uses Connect RPC with gRPC reflection enabled.

### ClickHouse Events Table

- **Engine:** `ReplacingMergeTree(insert_time)` — on merge, keeps the row with the highest `insert_time` per dedup key. Always query with `SELECT ... FINAL` to deduplicate at read time.
- **Dedup key (ORDER BY):** `(project_id, toStartOfMinute(occur_time), kind, event_id)` — minute granularity matches the finest time resolution dashboards use (per-minute charts). Full-precision `occur_time` is stored in the column.
- **Partitioning:** `PARTITION BY toYYYYMM(occur_time)` — ReplacingMergeTree **never** deduplicates across partitions.
- **occur_time stability:** `occur_time` is required (enforced by proto validation). Clients must send a stable value on retries — a different value that crosses a minute boundary lands in a different sort-key bucket (dedup fails); if it crosses a month boundary it lands in a different partition (permanent duplicate).

## Code Style

- Standard Go conventions. Use slog for logging. Run `go fmt ./...` after each change. A PostToolUse hook auto-runs `goimports` on every `.go` file edit.
- Always use context-aware slog variants (`slog.InfoContext`, `slog.ErrorContext`, `slog.WarnContext`, `slog.DebugContext`) instead of `slog.Info`, `slog.Error`, etc.
- Always use `slogx.Error(err)` (from `internal/slogx`) for logging errors. Never use `slog.Any("error", err)` or `slog.Any("err", err)`.
