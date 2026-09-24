SHELL := /bin/bash
BINARY := aoc
BUILD_DIR := ../../bin
# vendor/ contains only non-Go submodules (babysitter), so use module proxy.
export GOFLAGS ?= -mod=mod

# Build provenance (commit/tag/dirty/built_at) — shared with dist/build.sh and
# ./install via scripts/build-info.sh (single source of truth, see #302).
BUILD_INFO_SH := ../../scripts/build-info.sh
VERSION := $(shell $(BUILD_INFO_SH) | sed -n 's/^TAG=//p')
COMMIT := $(shell $(BUILD_INFO_SH) | sed -n 's/^COMMIT_SHORT=//p')
BUILT_AT := $(shell $(BUILD_INFO_SH) | sed -n 's/^BUILT_AT=//p')
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILT_AT)

.PHONY: build install test clean

build:
	CGO_ENABLED=0 go build -tags capsule -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY)-go .

install:
	CGO_ENABLED=0 go build -tags capsule -ldflags "$(LDFLAGS)" -o ~/.local/bin/$(BINARY) .

test:
	go test ./../../internal/...

clean:
	rm -f $(BUILD_DIR)/$(BINARY)-go
