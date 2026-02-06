# Cotton Project - QWEN Context

## Project Overview

Cotton is a modern, scalable backend service written in Go that provides a comprehensive platform for managing campaigns, subscriptions, user data, and delivery systems. The project uses ConnectRPC (gRPC-based protocol) for its API layer, PostgreSQL for primary data storage, NATS for messaging, and ClickHouse for analytics.

### Key Technologies

- **Go 1.25.7**: Main programming language
- **ConnectRPC**: gRPC-based API framework
- **PostgreSQL**: Primary relational database
- **NATS**: Messaging system with JetStream
- **ClickHouse**: Analytics database
- **Protocol Buffers**: API definition language
- **SQLC**: SQL code generation
- **Buf**: Protobuf ecosystem tooling

### Architecture Components

- **Authentication Service**: Email/password signup/signin with JWT tokens
- **Projects Service**: Dashboard-focused project management
- **Campaigns Service**: Campaign creation and management
- **Delivery Service**: Message delivery system
- **Users Service**: User data management (SDK-facing)
- **Subscriptions Service**: Subscription management (SDK-facing)

## Building and Running

### Prerequisites

- Go 1.25.7+
- Docker and Docker Compose
- Buf CLI
- SQLC

### Setup Commands

```bash
# Install dependencies
make install-all-deps

# Start infrastructure (PostgreSQL, NATS, ClickHouse)
make infra-up

# Generate code from protobuf definitions
make rpc

# Generate code from SQL queries
make sqlc

# Build all binaries
make build

# Run tests
make test
```

### Running Services

- Main server: `./bin/cotton-server`
- Worker services: `./bin/cotton-worker-*`
- Migration tools: `./bin/cotton-migrate-*`

### Development Workflow

1. Define API contracts in `.proto` files in the `proto/` directory
2. Run `make rpc` to generate Go code from protobuf definitions
3. Write SQL queries in the `schema/postgres/queries/` directory
4. Run `make sqlc` to generate Go database code
5. Implement business logic in the `internal/app/server/rpc/` directory
6. Build and test with `make build` and `make test`

## Project Structure

```
cotton/
├── cmd/                    # Application entry points
│   ├── cotton/             # Main CLI tool
│   ├── server/             # Main server binary
│   ├── migrate/            # Migration tools
│   └── workers/            # Background worker binaries
├── internal/               # Internal application code
│   └── app/                # Application logic
│       ├── server/         # Main HTTP/gRPC server
│       ├── migrate/        # Database migration logic
│       └── workers/        # Background job processors
├── proto/                  # Protocol buffer definitions
│   ├── auth/               # Authentication service
│   ├── campaigns/          # Campaign management
│   ├── delivery/           # Delivery service
│   ├── projects/           # Project management
│   ├── subscriptions/      # Subscription management
│   └── users/              # User management
├── schema/                 # Database schemas and migrations
│   └── postgres/           # PostgreSQL-specific schemas
├── infra/                  # Infrastructure configurations
│   └── dev/                # Development Docker Compose
├── internal/gen/           # Generated code
│   ├── proto/              # Generated protobuf code
│   └── repo/               # Generated SQL repository code
└── Makefile                # Build and development commands
```

## Development Conventions

### API Design

- APIs are defined using Protocol Buffers in the `proto/` directory
- Services follow a domain-driven design with separate packages for different concerns
- Validation rules are defined using protovalidate annotations

### Database Access

- SQL queries are defined in the `schema/postgres/queries/` directory
- SQLC generates type-safe database access code
- Separate read and write query sets for optimized performance

### Authentication

- Two authentication methods:
  - JWT tokens for dashboard access
  - API keys for SDK access
- Dual authentication available for shared services

### Environment Configuration

- Use `.env` file for local development (see `.env.example`)
- Environment variables for configuration using `go-envconfig`

## Testing

Run all tests with:

```bash
make test
```

## Infrastructure

The development infrastructure includes:

- PostgreSQL 18 for primary data storage
- NATS with JetStream for messaging and event streaming
- ClickHouse 23.3 for analytics and reporting
