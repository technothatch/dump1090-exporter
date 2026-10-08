# Convenience wrappers around the exact commands CI runs.
# Every command is also documented in README.md — `make` is optional.

.PHONY: build test lint fmt vet check help

help: ## List available targets
	@grep -E '^[a-z]+:.*##' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'

build: ## Compile all packages
	go build ./...

test: ## Run the test suite with the race detector and coverage
	go test -race -coverprofile coverage.out -covermode atomic ./...
	go tool cover -func coverage.out

fmt: ## Fail if any file needs gofmt
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet: ## Run go vet
	go vet ./...

lint: fmt vet ## gofmt + go vet + golangci-lint (requires golangci-lint on PATH)
	golangci-lint run

check: lint test build ## Everything CI runs
