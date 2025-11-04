.PHONY: install-go-deps
install-go-deps:
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
	go install github.com/pseudomuto/protoc-gen-doc/cmd/protoc-gen-doc@latest

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
	rm -rf internal/repo/gen
	sqlc generate

.PHONY: lint
lint:
	buf lint

.PHONY: rpc
rpc: lint
	buf generate

.PHONY: gen-ts
gen-ts: lint
	buf generate

.PHONY: build
build:
	go build -o bin/cotton main.go

.PHONY: test
test:
	go test ./...

.PHONY: psql
psql:
	docker compose -f infra/dev/docker-compose.yaml exec postgres psql -U postgres -d cotton

.PHONY: infra
infra:
	docker compose -f infra/dev/docker-compose.yaml up -d
