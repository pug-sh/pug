# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Cotton is a push notification platform built with Go (backend), React/TypeScript (frontend), and uses PostgreSQL, ClickHouse, and Apache Pulsar for data storage and messaging.

## Build & Run Commands

```bash
# Build the Go binary
make build

# Run tests
make test

# Start dev infrastructure (PostgreSQL, Pulsar, ClickHouse, Flink)
make infra-up

# Stop infrastructure
make infra-down

# Run database migrations
./bin/cotton postgres migrate
./bin/cotton pulsar migrate
./bin/cotton clickhouse migrate

# Start development server + workers together
./bin/cotton dev

# Start server only
./bin/cotton server

# Start individual workers
./bin/cotton worker subscription
```

### Code Generation

```bash
# Generate sqlc queries (after modifying SQL files)
make sqlc

# Generate protobuf code (after modifying .proto files)
make rpc

# Lint proto files
make lint
```

### Frontend (web/)

```bash
cd web
pnpm install
pnpm dev       # Start dev server
pnpm build     # Build for production
pnpm lint      # Run eslint
pnpm lint:fix  # Fix eslint issues
```

## Architecture

### Backend (Go)

The backend follows a layered architecture with Connect RPC (HTTP/2):

- **`internal/commands/`** - CLI entry points using Cobra (server, worker, dev, migrations)
- **`internal/rpc/`** - RPC handlers that map proto services to business logic
- **`internal/core/`** - Business logic layer with service and repo per domain (auth, campaigns, delivery, projects, segments, subscriptions)
- **`internal/workers/`** - Pulsar message consumers (campaigns, subscriptions)
- **`internal/gen/`** - Generated code (do not edit manually)
  - `proto/` - Generated from .proto files via buf
  - `repo/dbread/`, `repo/dbwrite/` - Generated from SQL via sqlc

### Database Pattern

PostgreSQL uses read/write separation:
- Queries in `schema/postgres/queries/read/` generate to `internal/gen/repo/dbread/`
- Queries in `schema/postgres/queries/write/` generate to `internal/gen/repo/dbwrite/`

### Proto/RPC

Services defined in `proto/` directory. Generated code goes to `internal/gen/proto/`. Uses Connect RPC with gRPC reflection enabled.

### Frontend (React)

Located in `web/`. Uses:
- React 19 with Vite
- TanStack Form for forms
- Wouter for routing
- Jotai for state management
- Connect RPC client for API calls
- shadcn/ui components in `web/src/components/ui/` (do not modify these)

## Code Style

- **Frontend**: Use pnpm as package manager. Use TanStack Form for forms. Do not modify `web/src/components/ui/` components.
- **Backend**: Standard Go conventions. Use slog for logging.
