BINARY      := darkorbit-downloader
MODULE      := github.com/Nosvemos/Darkorbit-Resource-Downloader
CMD         := ./cmd/darkorbit-downloader
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
EXE         := $(shell go env GOEXE)
LDFLAGS     := -s -w -X $(MODULE)/internal/app.Version=$(VERSION)
COVERPROFILE := cover.out

.DEFAULT_GOAL := help

.PHONY: help build test cover lint fmt tidy clean

help: ## Show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the binary into bin/
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY)$(EXE) $(CMD)

test: ## Run tests with the race detector
	go test -race -count=1 ./...

cover: ## Run tests and print a coverage summary
	go test -count=1 -coverprofile=$(COVERPROFILE) ./...
	go tool cover -func=$(COVERPROFILE)

lint: ## Run golangci-lint
	golangci-lint run ./...

fmt: ## Format code (gofmt + goimports via golangci-lint)
	golangci-lint fmt ./...

tidy: ## Tidy go.mod and go.sum
	go mod tidy

clean: ## Remove build and coverage outputs
	rm -rf bin dist $(COVERPROFILE)
