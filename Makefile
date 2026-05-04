.PHONY: install-go-deps
install-go-deps:
	go install tool

.PHONY: install-js-deps
install-js-deps:
	npm install -g @bufbuild/protoc-gen-es

.PHONY: install-dart-deps
install-dart-deps:
	dart pub global activate protoc_plugin

.PHONY: install-swift-deps
install-swift-deps:
	brew install swift-protobuf || echo "Please install swift-protobuf manually"

.PHONY: install-java-deps
install-java-deps:
	@echo "Please download the appropriate protoc-gen-grpc-java binary for your platform from:"
	@echo "https://repo1.maven.org/maven2/io/grpc/protoc-gen-grpc-java/"
	@echo "And place it in your PATH"

.PHONY: install-all-deps
install-all-deps: install-go-deps
	@echo "========================================"
	@echo "Additional SDK generation dependencies:"
	@echo "========================================"
	@echo "For TypeScript SDK generation:"
	@echo "  npm install -g protoc-gen-es"
	@echo ""
	@echo "For Dart SDK generation:"
	@echo "  dart pub global activate protoc_plugin"
	@echo ""
	@echo "For Swift SDK generation:"
	@echo "  On macOS: brew install swift-protobuf"
	@echo "  On Linux: Follow installation instructions from https://github.com/apple/swift-protobuf"
	@echo ""
	@echo "For Java SDK generation:"
	@echo "  Download from https://repo1.maven.org/maven2/io/grpc/protoc-gen-grpc-java/"
	@echo "  Or include in your Maven/Gradle build as a plugin"
	@echo "========================================"
	
.PHONY: sqlc
sqlc:
	rm -rf internal/gen/repo
	go tool sqlc generate

.PHONY: lint-proto
lint-proto:
	go tool buf lint

.PHONY: lint
lint:
	go tool golangci-lint run --timeout 5m ./...

.PHONY: rpc
rpc: lint-proto
	go tool buf generate

.PHONY: gen-ts
gen-ts: lint-proto
	go tool buf generate

.PHONY: build
build:
	go build -o bin/pug ./cmd/pug
	go build -o bin/pug-migrate-clickhouse ./cmd/migrate/clickhouse
	go build -o bin/pug-migrate-nats ./cmd/migrate/nats
	go build -o bin/pug-migrate-postgres ./cmd/migrate/postgres
	go build -o bin/pug-server ./cmd/server
	go build -o bin/pug-worker-campaign ./cmd/workers/campaign
	go build -o bin/pug-worker-device ./cmd/workers/device
	go build -o bin/pug-worker-events ./cmd/workers/events
	go build -o bin/pug-worker-profile-identify ./cmd/workers/profile/identify
	go build -o bin/pug-worker-profile-alias ./cmd/workers/profile/alias
	go build -o bin/pug-worker-profile-upsert ./cmd/workers/profile/upsert
	go build -o bin/pug-worker-scheduler ./cmd/workers/scheduler

.PHONY: fmt
fmt:
	rg --files -g '*.go' -g '!internal/gen/**' | xargs -r goimports -w

.PHONY: test
test:
	go test ./...

.PHONY: psql
psql:
	docker compose -f infra/dev/docker-compose.yaml exec postgres psql -U postgres -d pug

.PHONY: chql
chql:
	docker compose -f infra/dev/docker-compose.yaml exec clickhouse clickhouse-client -d pug

.PHONY: infra infra-up infra-up-fg infra-down
infra: infra-up

infra-up:
	docker compose -f infra/dev/docker-compose.yaml up -d

infra-up-fg:
	docker compose -f infra/dev/docker-compose.yaml up

infra-down:
	docker compose -f infra/dev/docker-compose.yaml down

.PHONY: clickstack clickstack-down
clickstack:
	docker compose -f infra/dev/docker-compose.yaml -f infra/dev/docker-compose.clickstack.yaml up -d

clickstack-down:
	docker compose -f infra/dev/docker-compose.yaml -f infra/dev/docker-compose.clickstack.yaml down
