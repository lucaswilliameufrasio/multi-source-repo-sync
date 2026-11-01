SHELL := /bin/sh

# Tooling: mise when available so local runs match the pinned toolchain.
ifneq (,$(shell command -v mise 2>/dev/null))
GO ?= mise exec -- go
GOLANGCI_LINT ?= mise exec -- golangci-lint
GORELEASER ?= mise exec -- goreleaser
else
GO ?= go
GOLANGCI_LINT ?= golangci-lint
GORELEASER ?= goreleaser
endif
GIT_CLIFF ?= git-cliff
VERSION ?= dev

.PHONY: help fmt fmt-check vet lint test build quality release-check snapshot changelog

help: ## list the targets
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/ -/'

fmt: ## format the code
	$(GO) fmt ./...

fmt-check: ## fail when something is not formatted
	@test -z "$$($(GO) fmt ./... )" || { echo "run make fmt"; exit 1; }

vet: ## go vet
	$(GO) vet ./...

lint: ## golangci-lint
	$(GOLANGCI_LINT) run ./...

test: ## the whole test suite (it uses the real git, never the network or GitHub)
	$(GO) test -count=1 ./...

build: ## build the binary into ./bin
	$(GO) build -ldflags "-X github.com/lucaswilliameufrasio/multi-source-repo-sync/internal/cli.Version=$(VERSION)" -o bin/multi-source-repo-sync ./cmd/multi-source-repo-sync

quality: fmt-check vet lint test ## the gate before a commit or a pull request

release-check: ## check the release configuration
	$(GORELEASER) check

snapshot: ## build every release archive locally, without publishing
	$(GORELEASER) release --snapshot --clean --skip=publish

changelog: ## regenerate CHANGELOG.md for VERSION (for example make changelog VERSION=0.2.0)
	$(GIT_CLIFF) --bump --tag "v$(VERSION)" -o CHANGELOG.md
