# Pyre build. Owned by the integration owner (WP00).
# Uses ~/sdk/go1.27.1 when present; override with GO=...
GO ?= $(shell test -x $(HOME)/sdk/go1.27.1/bin/go && echo $(HOME)/sdk/go1.27.1/bin/go || echo go)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/jhh3/pyre/internal/buildinfo.Version=$(VERSION)
BINS := pyre pyred pyre-hub pyre-mockhub
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64
DISTDIR ?= dist

# Supply-chain checks.
# govulncheck is pinned by the `tool` directive in go.mod. golangci-lint is
# deliberately NOT a go.mod tool: its dependency graph would raise modules the
# shipped binaries link (cobra, pflag, toml), so it is installed with
# `go install pkg@version` (its own module graph) into $(TOOLSDIR).
TOOLSDIR := bin/tools
VULN_GOOS ?= linux darwin
LINT_GOOS ?= linux darwin
GOLANGCI_LINT_VERSION ?= v2.14.0
REPRO_PLATFORMS ?= $(PLATFORMS)
VERIFY_REPRO_ARGS ?=
# Tools that shell out to `go` (golangci-lint) must find the same toolchain.
GOROOT_BIN = $(shell $(GO) env GOROOT)/bin

.PHONY: all help build test vet fmt fmt-check lint check fixtures schemas check-generated dist clean docs-check vuln verify-repro verify-repro-dist tools tools-test

all: build ## Build the binaries into bin/ (default)

help: ## List the targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build pyre, pyred, pyre-hub, pyre-mockhub into bin/
	@mkdir -p bin
	@for b in $(BINS); do CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$$b ./cmd/$$b || exit 1; done

test: ## Run the Go test suite
	$(GO) test ./...

# All analyzers run by default, including stdversion and tests. Do not pass
# analyzer flags here: naming any analyzer disables the rest.
vet: ## go vet with all analyzers
	$(GO) vet ./...

fmt: ## gofmt -w every Go package
	$(GOROOT_BIN)/gofmt -l -w $$($(GO) list -f '{{.Dir}}' ./...)

fmt-check: ## Fail if any Go file is not gofmt-clean
	@out=$$($(GOROOT_BIN)/gofmt -l $$($(GO) list -f '{{.Dir}}' ./...)); \
	if [ -n "$$out" ]; then echo "gofmt needed:" >&2; echo "$$out" >&2; exit 1; fi

# Gate (CI job "lint"): bug-finding analyzers per .golangci.yml, once per
# target OS because build tags differ. The tool is built for the host.
lint: ## golangci-lint gate (pinned; linux and darwin build tags)
	@mkdir -p $(TOOLSDIR)
	@GOBIN=$(CURDIR)/$(TOOLSDIR) GOOS=$$($(GO) env GOHOSTOS) GOARCH=$$($(GO) env GOHOSTARCH) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@for os in $(LINT_GOOS); do echo "==> golangci-lint GOOS=$$os"; PATH="$(GOROOT_BIN):$$PATH" GOOS=$$os $(TOOLSDIR)/golangci-lint run ./... || exit 1; done

check: fmt-check vet lint test ## Local pre-merge gate: fmt-check, vet, lint, test

fixtures: ## Regenerate contract and mock fixtures
	$(GO) run ./tools/genfixtures
	$(GO) test ./internal/runtime/mock -run TestDemoFleetFixture -update-demo >/dev/null

schemas: ## Regenerate JSON schemas
	$(GO) run ./tools/genschema

# CI: generated fixtures/schemas must match committed files.
check-generated: fixtures schemas ## Fail if generated fixtures/schemas differ from committed files
	git diff --exit-code -- tests/fixtures/contracts tests/fixtures/mock schemas

docs-check: ## Validate planning documents and the work graph
	python3 scripts/check_docs.py

dist: ## Cross-compile release binaries into dist/ (DISTDIR) with SHA256SUMS
	@mkdir -p $(DISTDIR)
	@for p in $(PLATFORMS); do os=$${p%/*}; arch=$${p#*/}; \
	  for b in $(BINS); do \
	    CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DISTDIR)/pyre-$(VERSION)-$$os-$$arch/$$b ./cmd/$$b || exit 1; \
	  done; done
	@cd $(DISTDIR) && shasum -a 256 pyre-$(VERSION)-*/* > SHA256SUMS

# Reachable-vulnerability scan of every package, once per target OS (build
# tags differ). The tool is built for the host, then pointed at each GOOS.
# Exits non-zero only for vulnerabilities in code Pyre actually calls.
vuln: ## govulncheck (linux and darwin build tags)
	@mkdir -p $(TOOLSDIR)
	@GOOS=$$($(GO) env GOHOSTOS) GOARCH=$$($(GO) env GOHOSTARCH) $(GO) build -o $(TOOLSDIR)/govulncheck golang.org/x/vuln/cmd/govulncheck
	@for os in $(VULN_GOOS); do echo "==> govulncheck GOOS=$$os"; GOOS=$$os $(TOOLSDIR)/govulncheck ./... || exit 1; done

# Reproducibility of the shipped release: packaging/verify-repro.sh runs
# packaging/release.sh twice (the second with an empty GOCACHE) and requires
# byte-identical binaries, tarballs and SHA256SUMS. Unsigned by default; pass
# flags through, e.g. VERIFY_REPRO_ARGS="--signing-key FILE" or "--keep DIR".
# It rewrites dist/pyre-$(VERSION)-* and dist/SHA256SUMS.
verify-repro: ## Build the release twice (release.sh) and require identical artifacts
	GO="$(GO)" bash packaging/verify-repro.sh --version "$(VERSION)" $(VERIFY_REPRO_ARGS)

# Quicker binaries-only check: `make dist` twice into fresh temp dirs (the
# second with an empty GOCACHE) and identical SHA256SUMS. Override the
# platform set with REPRO_PLATFORMS="linux/amd64".
verify-repro-dist: ## Binaries-only reproducibility check (make dist twice)
	@a=$$(mktemp -d) && b=$$(mktemp -d) && c=$$(mktemp -d) && \
	trap 'rm -rf "$$a" "$$b" "$$c"' EXIT && \
	echo "==> build 1 (current GOCACHE) -> $$a" && \
	$(MAKE) --no-print-directory dist GO="$(GO)" VERSION="$(VERSION)" PLATFORMS="$(REPRO_PLATFORMS)" DISTDIR="$$a" && \
	echo "==> build 2 (empty GOCACHE) -> $$b" && \
	GOCACHE="$$c" $(MAKE) --no-print-directory dist GO="$(GO)" VERSION="$(VERSION)" PLATFORMS="$(REPRO_PLATFORMS)" DISTDIR="$$b" && \
	if cmp -s "$$a/SHA256SUMS" "$$b/SHA256SUMS"; then \
	  echo "==> reproducible: $$(wc -l <"$$a/SHA256SUMS" | tr -d ' ') binaries identical"; cat "$$a/SHA256SUMS"; \
	else \
	  echo "==> NOT reproducible:" >&2; diff "$$a/SHA256SUMS" "$$b/SHA256SUMS" >&2; exit 1; \
	fi

# Bundled tools (tools/hive, tools/outfit, tools/rota). Outfit is its own Go
# module (no go.work: it would merge module graphs and move the versions linked
# into the Pyre binaries), so it is built with `go -C tools/outfit`.
tools: ## Build and stage Hive, Outfit and Rota per platform into dist/tools/
	GO="$(GO)" bash packaging/build-tools.sh --version "$(VERSION)" --out "$(DISTDIR)/tools" --platforms "$(PLATFORMS)"

tools-test: ## Run the bundled tools' own tests (Outfit go test; Hive pytest; Rota unittest)
	$(GO) -C tools/outfit test ./...
	python3 -m pytest -q -p no:cacheprovider tools/hive/tests
	python3 -m unittest discover -s tools/rota -p "test_*.py"

clean: ## Remove bin/ and dist/
	rm -rf bin dist
