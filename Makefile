.PHONY: gen proto-check build run test check check-local template-check

gen:
	buf lint
	buf generate
proto-check:
	buf lint
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/server ./cmd/server
run:
	go run ./cmd/server -config config/env/env.yml
test:
	go test ./...
check: check-local template-check
check-local:
	go build ./...
	golangci-lint run ./...
	go vet ./...
	go test -race -count=1 -timeout=5m ./...
	bash scripts/check-modern.sh
	bash scripts/check-architecture.sh
	buf lint
template-check:
	$(MAKE) -C templates/service check
