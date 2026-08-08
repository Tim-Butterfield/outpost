# Local build and install targets for outpost.
#
# Release archives are produced by goreleaser (.goreleaser.yml); this
# Makefile covers developer installs and the cross-compiled
# bin/<os>-<arch>/ layout that the responder deployment examples in
# README.md point OUTPOST_BIN at. Build flags mirror .goreleaser.yml so
# a local binary matches a released one.

GO      ?= go
PKG     := ./cmd/outpost

# Derived from the nearest tag (v1.0.1 -> 1.0.1), with -dirty appended
# for uncommitted trees. Override for a one-off: make install VERSION=x.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//')
ifeq ($(strip $(VERSION)),)
VERSION := 0.0.0-dev
endif

LDFLAGS := -s -w -X main.binaryVersion=$(VERSION)

# Platforms mirrored into bin/ by `make build-all`.
PLATFORMS ?= darwin-arm64 linux-arm64 windows-arm64

# Race detector is unsupported on windows/arm64; override with RACE=.
RACE ?= -race

export CGO_ENABLED = 0

.DEFAULT_GOAL := help

.PHONY: help
help:
	@echo "outpost $(VERSION)"
	@echo
	@echo "  make install     install outpost into \$$(go env GOPATH)/bin"
	@echo "  make build       build ./outpost for the host platform"
	@echo "  make build-all   cross-compile into bin/<os>-<arch>/"
	@echo "                   (PLATFORMS=$(PLATFORMS))"
	@echo "  make test        go test $(RACE) ./..."
	@echo "  make vet         go vet ./..."
	@echo "  make cover       coverage report over internal/ and pkg/"
	@echo "  make check       verify .goreleaser.yml"
	@echo "  make snapshot    goreleaser dry-run into dist/"
	@echo "  make clean       remove bin/, dist/, coverage.out"

.PHONY: install
install:
	$(GO) install -ldflags "$(LDFLAGS)" $(PKG)

.PHONY: build
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o outpost $(PKG)

.PHONY: build-all
build-all: $(PLATFORMS)

.PHONY: $(PLATFORMS)
$(PLATFORMS):
	GOOS=$(word 1,$(subst -, ,$@)) GOARCH=$(word 2,$(subst -, ,$@)) \
	  $(GO) build -ldflags "$(LDFLAGS)" \
	  -o bin/$@/outpost$(if $(filter windows-%,$@),.exe,) $(PKG)

.PHONY: test
test:
	$(GO) test $(RACE) -timeout 5m ./...

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: cover
cover:
	$(GO) test $(RACE) -timeout 5m -coverprofile=coverage.out \
	  -covermode=atomic ./internal/... ./pkg/outpost/...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: check
check:
	goreleaser check

.PHONY: snapshot
snapshot:
	goreleaser release --snapshot --clean

.PHONY: clean
clean:
	rm -rf bin dist coverage.out outpost
