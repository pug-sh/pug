.PHONY: install-go-deps
install-go-deps:
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
	
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
