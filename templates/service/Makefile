.PHONY: run build check gen proto-check
run:
	go run ./cmd/server -config config/env/env.yml
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o bin/server ./cmd/server
gen:
	buf lint
	buf generate
proto-check:
	buf lint
check:
	go build ./...
	golangci-lint run ./...
	go vet ./...
	go test -race -count=1 -timeout=5m ./...
	bash scripts/check-modern.sh
	buf lint
