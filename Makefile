# SPDX-License-Identifier: Apache-2.0
# Tools come from `nix develop` (see flake.nix) or your own PATH.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build test lint security fmt tidy gen check snapshot repro coverage fuzz perf version release-prep site install-test
.DEFAULT_GOAL := build

build: ## static binary in ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/repo-keeper ./cmd/repo-keeper

test: ## race detector + coverage
	go test -race -cover -coverprofile=coverage.out ./...

coverage: test ## per-package coverage floors; 100 % on the deletion safety predicate
	./scripts/coverage-gate.sh coverage.out

fuzz: ## run every fuzz target (FUZZTIME=15s each by default)
	./scripts/fuzz.sh

perf: build ## 500-repo synthetic fleet vs. the NFR-1/NFR-3 budgets
	N=500 ./scripts/perf.sh

lint:
	golangci-lint run ./...
	cd tools/site && golangci-lint run --config ../../.golangci.yml ./...

security: ## vulnerabilities + static security analysis
	govulncheck ./...
	gosec -quiet ./...
	cd tools/site && govulncheck ./... && gosec -quiet ./...

fmt:
	gofmt -w .

tidy:
	go mod tidy

gen: ## generated artifacts (config schema/docs arrive in M1+)
	@echo "nothing to generate yet"

snapshot: ## local release build (tarballs, deb, rpm) into ./dist without signing/publishing
	SOURCE_DATE_EPOCH=$$(git log -1 --format=%ct) goreleaser release --snapshot --clean --skip=sign,sbom,publish

repro: ## prove the release artifacts are byte-reproducible
	./scripts/check-reproducible.sh

check: coverage lint security ## what CI and every PR must pass

version: ## check that VERSION, the changelog and the flake agree
	./scripts/check-version.sh

release-prep: ## bump VERSION and the changelog heading: make release-prep NEW=0.1.0-beta.6
	@test -n "$(NEW)" || { echo "usage: make release-prep NEW=<version>"; exit 2; }
	./scripts/release-prep.sh $(NEW)

site: build ## build the documentation site into ./_site (the pipeline publishes it)
	cd tools/site && go test ./... && go run . -repo ../.. -bin ../../bin/repo-keeper $(if $(wildcard coverage.out),-coverage ../../coverage.out) -out ../../_site

install-test: ## shellcheck the scripts and run the installer tests against a synthetic release
	shellcheck -S warning scripts/*.sh
	./scripts/test-install.sh
