# Common tasks. Run `make help`.
BIN := bin/server
PKG := ./cmd/server

.PHONY: help run build test vet fmt check loadtest docker clean

help: ## show this help
	@grep -E '^[a-z]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-9s %s\n", $$1, $$2}'

run: ## run locally (set ADMIN_PASSWORD and JWT_SECRET first)
	go run $(PKG)

build: ## build a static binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) $(PKG)

test: ## run all tests
	go test ./...

vet: ## static checks
	go vet ./...

fmt: ## format the code
	gofmt -w .

check: ## what CI runs: formatting, vet, tests
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)
	go vet ./...
	go test ./...

loadtest: ## simulate many users against a running server (see README)
	go run ./cmd/loadtest -users 50000 -conns 9000 -sse 4000 -think 15s

docker: ## build the container image
	docker build -f deploy/Dockerfile -t restaurant-billing-backend .

clean: ## remove build output
	rm -rf bin
