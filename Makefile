APP := stardust
VERSION ?= dev
GO ?= go
GOLANGCI_LINT_VERSION ?= v2.14.0

.PHONY: build run test test-install lint fmt release

build:
	mkdir -p bin
	$(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/$(APP) ./cmd/stardust

run:
	$(GO) run ./cmd/stardust

test:
	$(GO) test ./...

test-install:
	PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -v

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run; else $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run; fi

fmt:
	$(GO)fmt -w $$(find . -type f -name '*.go' -not -path './vendor/*')

release:
	@if command -v goreleaser >/dev/null 2>&1; then goreleaser release --snapshot --clean; else echo "goreleaser is not installed; install it from https://goreleaser.com/install/"; exit 1; fi
