.PHONY: gen proto-check copy-check build run test check check-local

gen:
	buf lint
	buf generate
proto-check:
	bash scripts/check-proto.sh
copy-check:
	bash scripts/check-copy.sh
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/server ./cmd/server
run:
	go run ./cmd/server -config config/env/env.yml
test:
	go test ./...
check: check-local
check-local:
	go build ./...
	golangci-lint run ./...
	go vet ./...
	go test -race -count=1 -timeout=5m ./...
	bash scripts/check-modern.sh
	bash scripts/check-architecture.sh
	$(MAKE) proto-check copy-check
