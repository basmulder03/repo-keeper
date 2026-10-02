# SPDX-License-Identifier: Apache-2.0
# Tools come from `nix develop` (see flake.nix) or your own PATH.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build test lint security fmt tidy gen check
.DEFAULT_GOAL := build

build: ## static binary in ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/repo-keeper ./cmd/repo-keeper

test: ## race detector + coverage
	go test -race -cover -coverprofile=coverage.out ./...

lint:
	golangci-lint run ./...

security: ## vulnerabilities + static security analysis
	govulncheck ./...
	gosec -quiet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

gen: ## generated artifacts (config schema/docs arrive in M1+)
	@echo "nothing to generate yet"

check: test lint security ## what CI and every PR must pass
